package main

import (
	"path/filepath"
	// Embedded zoneinfo: the static Pi binary must render ticket times in the
	// location timezone even where the OS has no tz database.
	_ "time/tzdata"

	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"quiccpos/agent/internal/application/catchup"
	"quiccpos/agent/internal/application/driverreset"
	"quiccpos/agent/internal/application/escalation"
	"quiccpos/agent/internal/application/floorsync"
	orderApp "quiccpos/agent/internal/application/order"
	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/application/retention"
	ssecontroller "quiccpos/agent/internal/application/sseclient"
	"quiccpos/agent/internal/config"
	"quiccpos/agent/internal/domain/order"
	domainprinter "quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/infra/mainclient"
	"quiccpos/agent/internal/infra/notify"
	"quiccpos/agent/internal/infra/printer/escpos"
	"quiccpos/agent/internal/infra/printer/receipt"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/migrate"
	"quiccpos/agent/internal/observability"
	agentrpc "quiccpos/agent/internal/rpc"
	"quiccpos/agent/internal/transport"

	"github.com/rs/zerolog"
	"gopkg.in/lumberjack.v2"
)

// version + commit are injected at build time via -ldflags. CI passes them
// from VERSION (semver) + the git sha so the running binary can be
// inspected by ops tools.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	bootstrap := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().Logger()

	cfg, err := config.Load()
	if err != nil {
		bootstrap.Fatal().Err(err).Msg("Failed to load config")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --- Observability ----------------------------------------------------
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

	// --- Logger -----------------------------------------------------------
	var writer io.Writer
	switch cfg.LogOutput {
	case "json":
		writer = os.Stdout
	case "file":
		if err := os.MkdirAll("log", 0o755); err != nil {
			bootstrap.Fatal().Err(err).Msg("Failed to create log directory")
		}
		writer = &lumberjack.Logger{
			Filename: "log/agent.log", MaxSize: 100, MaxBackups: 10, MaxAge: 30, Compress: true,
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

	// --- Database ---------------------------------------------------------
	logger.Info().Ctx(ctx).Str("db_path", cfg.AgentDBPath).Msg("Running database migrations")
	// SQLite can't create the file's directory (default ~/.quicc on a fresh Pi).
	if err := os.MkdirAll(filepath.Dir(cfg.AgentDBPath), 0o700); err != nil {
		logger.Fatal().Err(err).Msg("Failed to create the database directory")
	}
	if err := migrate.Run(ctx, cfg.AgentDBPath); err != nil {
		logger.Fatal().Err(err).Msg("Failed to run database migrations")
	}
	logger.Info().Ctx(ctx).Msg("Database migrations complete")

	db, err := database.Open(ctx, cfg.AgentDBPath)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to open SQLite database")
	}
	defer db.Close()
	logger.Info().Ctx(ctx).Msg("Connected to database")

	orderRepo := repositories.NewOrderRepository(db, logger)
	settingsRepo := repositories.NewSettingsRepository(db, logger)
	storeConfigRepo := repositories.NewStoreConfigRepository(db, logger)
	deviceRepo := repositories.NewDeviceRepository(db)

	// --- Notifier ---------------------------------------------------------
	notifier := notify.NewNotifier(cfg.PushoverAppToken, cfg.PushoverUserKey, cfg.PushoverBossKey)
	if err := notifier.Send(ctx, "Agent started", "classical"); err != nil {
		logger.Warn().Err(err).Msg("Startup notification failed — continuing")
	}

	// --- Printer roster + dispatcher --------------------------------------
	// Roster is dynamic: rebuilt on every store_config SSE handshake. On boot
	// we hydrate from the DB so the agent can keep printing through a restart
	// even while main/ is unreachable.
	printerRegistry := printerApp.NewRegistry()
	factory := func(name, ip string) domainprinter.Printer {
		return escpos.New(ip, name, logger)
	}
	// --- In-memory store_context holder (drives the receipt builder) ------
	// atomic.Value gives us lock-free reads on the hot Print path with a
	// single Store on the cool reconnect path.
	var storeCtx atomic.Value // storeconfig.StoreContext
	storeCtx.Store(storeconfig.StoreContext{})

	// receiptBuilder closes over the atomic so a hot-swap propagates without
	// re-wiring the printer service.
	receiptBuilder := func(snap order.Snapshot, t domainprinter.Ticket) []byte {
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		return receipt.Build(snap, sc, t)
	}

	dispatcher := printerApp.NewDispatcher(ctx, factory, printerRegistry, notifier, cfg.PrinterDetectDelay, receiptBuilder, logger, meters)

	if saved, err := storeConfigRepo.Get(ctx); err == nil && saved != nil {
		storeCtx.Store(*saved)
		dispatcher.Apply(ctx, saved.Printers)
		logger.Info().Ctx(ctx).
			Str("tenant", saved.Tenant.Name).
			Str("location", saved.Location.Name).
			Int("printers", len(saved.Printers)).
			Msg("store_config hydrated from DB")
	}

	// printFn plans the order's tickets (stations + receipt) and prints them.
	printFn := dispatcher.Execute

	// --- Order service + mobile-facing SSE broker -------------------------
	broker := ssebroker.New()
	orderService := orderApp.NewService(orderRepo, settingsRepo, printFn, notifier, broker, logger, meters)

	// --- Store_config SSE handshake sink ----------------------------------
	sink := &storeConfigApplier{
		repo:       storeConfigRepo,
		dispatcher: dispatcher,
		storeCtx:   &storeCtx,
		logger:     logger.With().Str("module", "storeconfig-sink").Logger(),
	}

	// --- SSE controller ---------------------------------------------------
	// The controller owns the SSE client goroutine and exposes SetKey so the
	// web UI can swap the key at runtime. The key comes ONLY from the local DB
	// (minted on-site via the web-UI login) — never seeded from env/Doppler.
	// --- Duplex RPC channel ------------------------------------------------
	// main→agent commands arrive as `event: command` SSE frames; agent→main
	// replies/reports POST back with the store key. The poster reads the
	// current key from the controller (set just below to break the cycle).
	mainClient := mainclient.New(cfg.MainServerURL)
	rpcPoster := &rpcReplyPoster{client: mainClient}
	registry := agentrpc.NewRegistry(rpcPoster, logger, meters)
	registry.Register(agentrpc.MsgPing, func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"pong": true, "version": version}, nil
	})
	// Printer roster: main can pull it on demand …
	registry.Register(agentrpc.MsgPrinterReportRequest, func(_ context.Context, _ json.RawMessage) (any, error) {
		return printerApp.BuildReport(printerRegistry, version), nil
	})

	sseCtrl := ssecontroller.New(ctx, cfg.MainServerURL, orderService, sink, registry, logger, meters)
	rpcPoster.key = sseCtrl.CurrentKey

	// Auto-reset on a definitive revoke: if main rejects our token (401 — the
	// device was revoked/hard-deleted on main), wipe the local provisioning
	// (token + password) and drop the SSE connection so the web UI falls back to
	// the "paste a token" setup screen. This fires from BOTH detection paths — the
	// SSE (re)connect AND any other agent→main request that gets 401 (the 30s
	// printer-report heartbeat, the history/store-config proxies, RPC replies) —
	// so a dashboard revoke resets the agent without waiting for an SSE reconnect.
	//
	// It's idempotent + key-checked: `usedKey` is the key that was rejected; we
	// only wipe if it still matches the stored key, so a stale in-flight 401 for a
	// since-rotated token can't nuke a token the operator just re-pasted, and a
	// second 401 after the wipe (key now "") is a no-op. A plain unreachable-main
	// is a connection error (not a 401), so a network blip never wipes.
	// onWipe is set once the HTTP server exists (it owns sessions + tablets).
	var onWipe atomic.Pointer[func(context.Context)]
	wipeProvisioning := func(usedKey string) {
		cur, err := settingsRepo.GetStoreAPIKey(context.Background())
		if err != nil {
			logger.Error().Err(err).Msg("revoke check: failed to read store key")
			return
		}
		if cur == "" || cur != usedKey {
			return // already reset, or the key was rotated — don't wipe a fresh token
		}
		logger.Warn().Msg("token rejected by main — wiping local provisioning, resetting to setup")
		if err := settingsRepo.ClearProvisioning(context.Background()); err != nil {
			logger.Error().Err(err).Msg("failed to clear provisioning after revoke")
		}
		if f := onWipe.Load(); f != nil {
			(*f)(context.Background()) // sign out web sessions + revoke paired tablets
		}
		sseCtrl.SetKey("")
	}
	sseCtrl.OnRejected(wipeProvisioning)
	// Orders whose SSE frame was lost when the stream dropped: fetch them
	// from main after every reconnect (OnArrival skips ones we already hold).
	sseCtrl.OnConnected(catchup.Hook(ctx, func(rc context.Context, path, rawQuery string) (int, []byte, error) {
		return mainClient.Get(rc, sseCtrl.CurrentKey(), path, rawQuery)
	}, orderService, orderRepo, logger))
	mainClient.SetOnUnauthorized(wipeProvisioning)

	// … and the agent pushes it proactively: on every store_config apply (the
	// roster just changed) and on a periodic heartbeat.
	reportPrinters := func(rc context.Context) {
		if !sseCtrl.Configured() {
			return
		}
		if err := registry.Report(rc, agentrpc.MsgPrinterReport, printerApp.BuildReport(printerRegistry, version)); err != nil {
			logger.Debug().Ctx(rc).Err(err).Msg("printer.report push failed")
		}
	}
	sink.postApply = reportPrinters
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reportPrinters(ctx)
			}
		}
	}()

	// Store key comes ONLY from the local DB — keys are minted on-site via the
	// web-UI login, never seeded from env/Doppler. Empty ⇒ unconfigured ⇒ the UI
	// shows the "set up this device" screen.
	initialKey, err := settingsRepo.GetStoreAPIKey(ctx)
	if err != nil {
		logger.Warn().Ctx(ctx).Err(err).Msg("Failed to load STORE_API_KEY from DB")
		initialKey = ""
	}
	if initialKey != "" {
		sseCtrl.SetKey(initialKey)
	} else {
		logger.Info().Ctx(ctx).Msg("No STORE_API_KEY yet — agent unconfigured. Set it up via the web UI login.")
	}

	// Resolve (or generate + persist) the web-UI session-signing secret so
	// manager sessions survive restarts (previously random per boot).
	sessionSecret, err := resolveSessionSecret(ctx, settingsRepo, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to resolve session secret")
	}

	// --- Wiring -----------------------------------------------------------
	// mainClient (built above for the duplex channel) also backs the web UI's
	// login + history + store-config proxy calls; session gates the web UI.
	session := transport.NewSessionManager(sessionSecret, func(hexSecret string) error {
		return settingsRepo.SetSessionSecret(context.Background(), hexSecret)
	})
	httpServer := transport.NewServer(orderService, broker, printerRegistry, settingsRepo, sseCtrl, mainClient, session, deviceRepo, cfg.HTTPPort, version, commit, logger)

	// Floor sync: every committed floor change wakes the sender, which pushes
	// the floor_events outbox to main as `floor.events` (retrying with backoff
	// while main is unreachable).
	floorSender := floorsync.New(orderRepo, func(rc context.Context, body []byte) (int, []byte, error) {
		k := sseCtrl.CurrentKey()
		if k == "" {
			return 0, nil, fmt.Errorf("no store key configured")
		}
		return mainClient.PostRPC(rc, k, body)
	}, logger, meters)
	orderService.OnFloorEvent(floorSender.Wake)

	resetAccess := httpServer.ResetAccess
	onWipe.Store(&resetAccess)
	go httpServer.Start(ctx)
	go retention.Run(ctx, orderRepo, cfg.KeepBatches, logger, meters)
	go floorSender.Run(ctx)
	go escalation.Run(ctx, orderService, logger)
	go driverreset.Run(ctx, orderService, func() *time.Location {
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		if l, err := time.LoadLocation(sc.Location.Timezone); err == nil && sc.Location.Timezone != "" {
			return l
		}
		return time.Local
	}, logger)

	<-ctx.Done()
	logger.Info().Msg("Agent shut down")
}

// rpcReplyPoster sends duplex-channel envelopes to main using the agent's
// current store key (read lazily from the SSE controller, so it always uses
// the live key even after a runtime key swap).
type rpcReplyPoster struct {
	client *mainclient.Client
	key    func() string
}

func (p *rpcReplyPoster) PostRPC(ctx context.Context, body []byte) error {
	k := ""
	if p.key != nil {
		k = p.key()
	}
	if k == "" {
		return fmt.Errorf("no store key configured")
	}
	status, _, err := p.client.PostRPC(ctx, k, body)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("agent rpc post: status %d", status)
	}
	return nil
}

// storeConfigApplier implements sseclient.StoreConfigSink. Every Apply call:
//
//  1. Stores the new store_context in the atomic value the receipt builder
//     and printFn read from.
//  2. Rebuilds the dispatcher's printer roster.
//  3. Persists the payload to SQLite so an agent restart keeps operating
//     even if main/ is offline.
//
// Persistence failures are non-fatal — the agent runs off the in-memory copy.
type storeConfigApplier struct {
	repo       storeconfig.Repository
	dispatcher *printerApp.Dispatcher
	storeCtx   *atomic.Value
	logger     zerolog.Logger
	// postApply, if set, runs after the roster is rebuilt (used to push a fresh
	// printer.report up to main).
	postApply func(context.Context)
}

func (a *storeConfigApplier) Apply(ctx context.Context, sc storeconfig.StoreContext) error {
	a.storeCtx.Store(sc)
	a.dispatcher.Apply(ctx, sc.Printers)
	if a.postApply != nil {
		a.postApply(ctx)
	}
	if err := a.repo.Save(ctx, sc); err != nil {
		a.logger.Warn().Ctx(ctx).Err(err).Msg("Failed to persist store_config (continuing with in-memory copy)")
		return nil
	}
	a.logger.Info().Ctx(ctx).
		Str("tenant", sc.Tenant.Name).
		Str("location", sc.Location.Name).
		Int("printers", len(sc.Printers)).
		Msg("store_config applied")
	return nil
}

// resolveSessionSecret loads the persisted web-UI session-signing secret,
// generating + persisting a fresh 32-byte one on first run. Persisting it is
// what keeps manager sessions valid across agent restarts.
func resolveSessionSecret(ctx context.Context, settings order.SettingsRepository, logger zerolog.Logger) ([]byte, error) {
	hexStr, err := settings.GetSessionSecret(ctx)
	if err != nil {
		logger.Warn().Ctx(ctx).Err(err).Msg("Failed to load session secret; generating a fresh one")
	}
	if hexStr != "" {
		if b, derr := hex.DecodeString(hexStr); derr == nil && len(b) >= 32 {
			return b, nil
		}
		logger.Warn().Ctx(ctx).Msg("Stored session secret invalid; regenerating")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate session secret: %w", err)
	}
	if err := settings.SetSessionSecret(ctx, hex.EncodeToString(b)); err != nil {
		logger.Warn().Ctx(ctx).Err(err).Msg("Failed to persist session secret; sessions won't survive restart")
	}
	return b, nil
}
