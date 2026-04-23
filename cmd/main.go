package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	orderApp "quiccpos/agent/internal/application/order"
	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/application/retention"
	"quiccpos/agent/internal/config"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/infra/notify"
	"quiccpos/agent/internal/infra/printer/escpos"
	sseclient "quiccpos/agent/internal/infra/sse"
	"quiccpos/agent/internal/migrate"
	"quiccpos/agent/internal/observability"
	"quiccpos/agent/internal/transport"

	"github.com/rs/zerolog"
	"gopkg.in/lumberjack.v2"
)

// version is set via -ldflags at build time ("-X main.version=<sha>").
// Defaults to "dev" for plain `go run`.
var version = "dev"

func main() {
	bootstrap := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().Logger()

	cfg, err := config.Load()
	if err != nil {
		bootstrap.Fatal().Err(err).Msg("Failed to load config")
	}

	// Root context: cancelled on SIGINT/SIGTERM. Every long-running goroutine
	// — HTTP server, SSE reader, KeepCheck loops, retention sweeper, OTEL
	// batch processors — honors it, so Ctrl-C drains cleanly.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --- Observability setup (must come before the logger is wrapped, so
	// the log bridge can pick up the global LoggerProvider.) -----------------
	shutdownOtel, err := observability.Setup(ctx, observability.Config{
		Endpoint:    cfg.OTELEndpoint,
		ServiceName: cfg.OTELServiceName,
		AppEnv:      cfg.AppEnv,
		Version:     version,
	})
	if err != nil {
		bootstrap.Error().Err(err).Msg("OTEL setup failed — continuing with no telemetry")
		shutdownOtel = func(context.Context) error { return nil }
	}
	defer func() {
		if err := shutdownOtel(context.Background()); err != nil {
			bootstrap.Warn().Err(err).Msg("OTEL shutdown reported errors")
		}
	}()

	meters, err := observability.NewMeters()
	if err != nil {
		bootstrap.Fatal().Err(err).Msg("Failed to create metric instruments")
	}

	// --- Logger (wrapped with trace-id/span-id hook + OTLP logs bridge) ----
	var writer io.Writer
	switch cfg.LogOutput {
	case "json":
		writer = os.Stdout
	case "file":
		if err := os.MkdirAll("log", 0o755); err != nil {
			bootstrap.Fatal().Err(err).Msg("Failed to create log directory")
		}
		writer = &lumberjack.Logger{
			Filename:   "log/agent.log",
			MaxSize:    100,
			MaxBackups: 10,
			MaxAge:     30,
			Compress:   true,
		}
	default:
		writer = zerolog.ConsoleWriter{Out: os.Stdout}
	}

	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil || cfg.LogLevel == "" {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	rawLogger := zerolog.New(writer).With().Timestamp().Logger()
	logger := observability.Wire(rawLogger, cfg.OTELServiceName)
	logger.Info().
		Str("log_level", level.String()).
		Str("app_env", cfg.AppEnv).
		Str("main_server_url", cfg.MainServerURL).
		Str("http_port", cfg.HTTPPort).
		Str("otel_endpoint", cfg.OTELEndpoint).
		Msg("Agent starting")

	// --- Database (migrate then connect) -----------------------------------
	logger.Info().Ctx(ctx).Msg("Running database migrations")
	if err := migrate.Run(ctx, cfg.DatabaseURL); err != nil {
		logger.Fatal().Err(err).Msg("Failed to run database migrations")
	}
	logger.Info().Ctx(ctx).Msg("Database migrations complete")

	pool, err := database.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to open DB pool")
	}
	defer pool.Close()
	logger.Info().Ctx(ctx).Msg("Connected to database")

	orderRepo := repositories.NewOrderRepository(pool, logger)
	settingsRepo := repositories.NewSettingsRepository(pool, logger)
	printerConfigRepo := repositories.NewPrinterConfigRepository(pool, logger)

	// --- Notifier + startup notification -----------------------------------
	notifier := notify.NewNotifier(cfg.PushoverAppToken, cfg.PushoverUserKey)
	if err := notifier.Send(ctx, "Agent started", "classical"); err != nil {
		logger.Warn().Err(err).Msg("Startup notification failed — continuing")
	}

	// --- Printers ----------------------------------------------------------
	// Env vars seed the DB if-absent; mobile-set overrides take precedence
	// and survive restarts. For each known printer we resolve the effective
	// IP: DB row if present, env-var otherwise (which may be empty → handle
	// stays unconfigured until mobile sets it).
	printerSeeds := []struct{ name, envIP string }{
		{"Online", cfg.PrinterIP},
		{"Pizza", cfg.PizzaPrinterIP},
		{"Desi", cfg.DesiPrinterIP},
		{"Sub", cfg.SubPrinterIP},
		{"Wings", cfg.WingsPrinterIP},
	}
	effectiveIPs := make(map[string]string, len(printerSeeds))
	for _, seed := range printerSeeds {
		if seed.envIP != "" {
			if err := printerConfigRepo.UpsertIfAbsent(ctx, seed.name, seed.envIP); err != nil {
				logger.Warn().Ctx(ctx).Err(err).Str("printer", seed.name).Msg("Failed to seed printer IP from env")
			}
		}
		pc, ok, err := printerConfigRepo.Get(ctx, seed.name)
		if err != nil {
			logger.Warn().Ctx(ctx).Err(err).Str("printer", seed.name).Msg("Failed to read printer IP from DB, falling back to env")
			effectiveIPs[seed.name] = seed.envIP
			continue
		}
		if ok {
			effectiveIPs[seed.name] = pc.IP
		} else {
			effectiveIPs[seed.name] = seed.envIP
		}
	}

	printerRegistry := printerApp.NewRegistry()

	pizzaPrinter := escpos.New(effectiveIPs["Pizza"], "Pizza", logger)
	desiPrinter := escpos.New(effectiveIPs["Desi"], "Desi", logger)
	subPrinter := escpos.New(effectiveIPs["Sub"], "Sub", logger)
	wingsPrinter := escpos.New(effectiveIPs["Wings"], "Wings", logger)
	onlinePrinter := escpos.New(effectiveIPs["Online"], "Online", logger)

	pizzaService := printerApp.NewService(pizzaPrinter, printerRegistry, logger, meters)
	desiService := printerApp.NewService(desiPrinter, printerRegistry, logger, meters)
	subService := printerApp.NewService(subPrinter, printerRegistry, logger, meters)
	wingsService := printerApp.NewService(wingsPrinter, printerRegistry, logger, meters)
	onlineService := printerApp.NewService(onlinePrinter, printerRegistry, logger, meters)

	printerManager := printerApp.NewManager(printerConfigRepo, []*printerApp.Service{
		pizzaService, desiService, subService, wingsService, onlineService,
	}, logger)

	go pizzaService.KeepCheck(ctx, cfg.PrinterDetectDelay, notifier)
	go desiService.KeepCheck(ctx, cfg.PrinterDetectDelay, notifier)
	go subService.KeepCheck(ctx, cfg.PrinterDetectDelay, notifier)
	go wingsService.KeepCheck(ctx, cfg.PrinterDetectDelay, notifier)
	go onlineService.KeepCheck(ctx, cfg.PrinterDetectDelay, notifier)

	// --- Order service + mobile-facing SSE broker --------------------------
	broker := ssebroker.New()
	orderService := orderApp.NewService(orderRepo, settingsRepo, onlineService, notifier, broker, logger)

	// --- SSE client (main/ → agent) + HTTP server (agent → mobile) --------
	sseClient := sseclient.New(cfg.MainServerURL, cfg.AgentAPIKey, orderService, logger, meters)
	httpServer := transport.NewServer(orderService, broker, printerRegistry, printerManager, cfg.HTTPPort, logger)

	go httpServer.Start(ctx)
	go sseClient.Start(ctx)
	go retention.Run(ctx, orderRepo, logger)

	<-ctx.Done()
	logger.Info().Msg("Agent shut down")
}
