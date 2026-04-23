package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	orderApp "quiccpos/agent/internal/application/order"
	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/config"
	"quiccpos/agent/internal/infrastructure/mainclient"
	"quiccpos/agent/internal/infrastructure/notify"
	"quiccpos/agent/internal/infrastructure/printer/escpos"
	sseclient "quiccpos/agent/internal/infrastructure/sse"
	"quiccpos/agent/internal/observability"
	"quiccpos/agent/internal/store"
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
	// — HTTP server, SSE reader, KeepCheck loops, OTEL batch processors —
	// honors it, so Ctrl-C drains cleanly.
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

	// --- Notifier + startup notification -----------------------------------
	notifier := notify.NewNotifier(cfg.PushoverAppToken, cfg.PushoverUserKey)
	if err := notifier.Send(ctx, "Agent started", "classical"); err != nil {
		logger.Warn().Err(err).Msg("Startup notification failed — continuing")
	}

	// --- Printers ----------------------------------------------------------
	pizzaPrinter := escpos.New(cfg.PizzaPrinterIP, "Pizza", logger)
	desiPrinter := escpos.New(cfg.DesiPrinterIP, "Desi", logger)
	subPrinter := escpos.New(cfg.SubPrinterIP, "Sub", logger)
	wingsPrinter := escpos.New(cfg.WingsPrinterIP, "Wings", logger)
	onlinePrinter := escpos.New(cfg.PrinterIP, "Online", logger)

	pizzaService := printerApp.NewService(pizzaPrinter, logger, meters)
	desiService := printerApp.NewService(desiPrinter, logger, meters)
	subService := printerApp.NewService(subPrinter, logger, meters)
	wingsService := printerApp.NewService(wingsPrinter, logger, meters)
	onlineService := printerApp.NewService(onlinePrinter, logger, meters)

	go pizzaService.KeepCheck(ctx, cfg.PrinterDetectDelay, notifier)
	go desiService.KeepCheck(ctx, cfg.PrinterDetectDelay, notifier)
	go subService.KeepCheck(ctx, cfg.PrinterDetectDelay, notifier)
	go wingsService.KeepCheck(ctx, cfg.PrinterDetectDelay, notifier)
	go onlineService.KeepCheck(ctx, cfg.PrinterDetectDelay, notifier)

	orderService := orderApp.NewService(onlineService, notifier, logger)

	// --- SSE client + HTTP server -----------------------------------------
	orderStore := store.New()
	mainClient := mainclient.New(cfg.MainServerURL, cfg.AgentAPIKey, logger)
	sseClient := sseclient.New(cfg.MainServerURL, cfg.AgentAPIKey, orderService, orderStore, mainClient, logger, meters)

	httpServer := transport.NewServer(orderStore, orderService, cfg.HTTPPort, logger)

	go httpServer.Start(ctx)
	go sseClient.Start(ctx)

	<-ctx.Done()
	logger.Info().Msg("Agent shut down")
}
