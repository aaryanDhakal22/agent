package main

import (
	"path/filepath"
	// Embedded zoneinfo: the static Pi binary must render ticket times in the
	// location timezone even where the OS has no tz database.
	_ "time/tzdata"

	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"quiccpos/agent/internal/application/acker"
	"quiccpos/agent/internal/application/appupdate"
	"quiccpos/agent/internal/application/autoclose"
	"quiccpos/agent/internal/application/backup"
	"quiccpos/agent/internal/application/catchup"
	"quiccpos/agent/internal/application/dbhealth"
	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/application/driverreset"
	"quiccpos/agent/internal/application/escalation"
	"quiccpos/agent/internal/application/floorsync"
	"quiccpos/agent/internal/application/health"
	"quiccpos/agent/internal/application/lateorders"
	"quiccpos/agent/internal/application/mapfile"
	orderApp "quiccpos/agent/internal/application/order"
	phoneApp "quiccpos/agent/internal/application/phonepay"
	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/application/printernet"
	"quiccpos/agent/internal/application/recovery"
	"quiccpos/agent/internal/application/retention"
	"quiccpos/agent/internal/application/revoke"
	ssecontroller "quiccpos/agent/internal/application/sseclient"
	"quiccpos/agent/internal/application/statesync"
	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/application/update"
	"quiccpos/agent/internal/application/watchdog"
	"quiccpos/agent/internal/config"
	"quiccpos/agent/internal/domain/order"
	domainprinter "quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/infra/discovery"
	"quiccpos/agent/internal/infra/mainclient"
	"quiccpos/agent/internal/infra/notify"
	"quiccpos/agent/internal/infra/printer/escpos"
	"quiccpos/agent/internal/infra/printer/receipt"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/migrate"
	"quiccpos/agent/internal/observability"
	agentrpc "quiccpos/agent/internal/rpc"
	"quiccpos/agent/internal/safego"
	"quiccpos/agent/internal/transport"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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
	procStart := time.Now()
	bootstrap := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().Logger()

	cfg, err := config.Load()
	if err != nil {
		bootstrap.Fatal().Err(err).Msg("Failed to load config")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --- Database (before telemetry: a linked Pi's secrets live in it) ------
	// SQLite can't create the file's directory (default ~/.quicc on a fresh Pi).
	if err := os.MkdirAll(filepath.Dir(cfg.AgentDBPath), 0o700); err != nil {
		bootstrap.Fatal().Err(err).Msg("Failed to create the database directory")
	}
	if err := migrate.Run(ctx, cfg.AgentDBPath); err != nil {
		bootstrap.Fatal().Err(err).Msg("Failed to run database migrations")
	}
	db, err := database.Open(ctx, cfg.AgentDBPath)
	if err != nil {
		bootstrap.Fatal().Err(err).Msg("Failed to open SQLite database")
	}
	defer db.Close()
	// S-7: an integrity check at start (a few ms on the Pi's DB). A problem
	// is logged now, paged and kept on the Pi page once the notifier exists.
	quickCheck, qcErr := database.QuickCheck(ctx, db)
	if qcErr != nil {
		quickCheck = qcErr.Error()
	}
	if quickCheck != "" {
		bootstrap.Error().Str("quick_check", quickCheck).Msg("SQLite quick_check failed: the database file is damaged")
	}
	// Secrets main handed over at link time fill whatever the environment
	// leaves empty (env always wins: dev/staging Doppler keep working).
	if li, err := repositories.NewSettingsRepository(db, bootstrap).GetLinkInfo(ctx); err == nil {
		applyLinkedSecrets(cfg, li.Secrets)
	}

	// --- Observability ----------------------------------------------------
	// The resource carries the linked store's tenant/location slugs (from the
	// persisted store_config; "unlinked" with no store key). It is fixed for
	// the process: a store linked later is labelled from the next start.
	resTenant, resLocation := persistedStoreSlugs(ctx, db, bootstrap)
	shutdownOtel, err := observability.Setup(ctx, observability.Config{
		Endpoint:    cfg.OTELEndpoint,
		ServiceName: cfg.OTELServiceName,
		AppEnv:      cfg.AppEnv,
		Version:     version,
		Tenant:      resTenant,
		Location:    resLocation,
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

	// CM-0: the OTLP bridge sits at the writer, so every field of a line
	// reaches Loki as a log attribute (a hook only sees the message).
	rawLogger := zerolog.New(observability.LogWriter(writer, cfg.OTELServiceName)).With().Timestamp().Logger()
	// Developer-mode events (GET /api/dev/events): floor events the services
	// add + every warn/error log line (its message only, never its fields).
	devRing := devlog.New(devlog.DefaultSize)
	logger := observability.Wire(rawLogger, cfg.OTELServiceName).Hook(devRing.Hook())
	logger.Info().
		Str("log_level", level.String()).
		Str("app_env", cfg.AppEnv).
		Str("main_server_url", cfg.MainServerURL).
		Str("http_port", cfg.HTTPPort).
		Str("otel_endpoint", cfg.OTELEndpoint).
		Msg("Agent starting")
	logger.Info().Ctx(ctx).Str("db_path", cfg.AgentDBPath).Msg("Database ready (migrations applied)")

	// Hardening F-1: recovered panics are logged through the agent's logger;
	// a fatal crash (an unrecovered panic, a runtime fatal error) is written
	// to crash/crash-<ts>.txt next to the DB (newest 5 kept) and reported
	// at the next start. F-6: 3 starts in 10 minutes = a crash loop (paged).
	safego.SetLogger(logger)
	crashDir := filepath.Join(filepath.Dir(cfg.AgentDBPath), "crash")
	crashes := safego.TakeNewCrashes(crashDir)
	for _, c := range crashes {
		logger.Error().Str("crash", c).Msg("The agent crashed last time")
	}
	if _, err := safego.SetCrashOutput(crashDir, time.Now()); err != nil {
		logger.Warn().Err(err).Msg("Crash file not set up")
	}
	crashLoop := safego.NoteStart(crashDir, time.Now())

	// R-1: Go runtime metrics (goroutines, memory, GC) + pprof on loopback
	// when PPROF_ADDR is set.
	if err := observability.StartRuntimeMetrics(); err != nil {
		logger.Warn().Err(err).Msg("Runtime metrics not started")
	}
	if _, err := observability.StartPprof(ctx, cfg.PprofAddr, logger); err != nil {
		logger.Warn().Err(err).Msg("pprof not started")
	}
	if cfg.DevTimersRefused != "" {
		logger.Warn().Msg(cfg.DevTimersRefused)
	} else if len(cfg.DevTimers) > 0 {
		logger.Warn().Interface("timers", cfg.DevTimers).Msg("dev timers: time-based jobs check more often (AGENT_DEV_TIMERS, dev only)")
	}

	orderRepo := repositories.NewOrderRepository(db, logger)
	settingsRepo := repositories.NewSettingsRepository(db, logger)
	storeConfigRepo := repositories.NewStoreConfigRepository(db, logger)
	deviceRepo := repositories.NewDeviceRepository(db)

	// --- Notifier ---------------------------------------------------------
	notifier := notify.NewNotifier(cfg.PushoverAppToken, cfg.PushoverUserKey, cfg.PushoverBossKey)
	notifier.SetAdminDevices(notify.ParseDevices(cfg.PushoverAdminDevices))
	// Outside prod, no admin device = no system/floor alerts at all: drivers'
	// phones share the account (2.4.1).
	notifier.SetStrictScope(cfg.AppEnv != "prod")
	if err := notifier.Send(ctx, "Agent started", "classical"); err != nil {
		logger.Warn().Err(err).Msg("Startup notification failed — continuing")
	}
	if quickCheck != "" {
		qc := quickCheck
		safego.Go("notify.quick_check", func() {
			if err := notifier.Send(context.WithoutCancel(ctx), "The Pi's database failed its integrity check at start: "+qc+
				". Keep taking orders; restore from the nightly backup after close (RECOVERY.md).", "siren"); err != nil {
				logger.Warn().Err(err).Msg("quick_check notification failed")
			}
		})
	}
	if crashLoop || len(crashes) > 0 {
		msg := "The Pi's agent crashed and restarted"
		if crashLoop {
			msg = "The Pi's agent is crash-looping (3+ starts in 10 minutes)"
		}
		if len(crashes) > 0 {
			msg += ": " + crashes[0]
		}
		safego.Go("notify.crash", func() {
			if err := notifier.Send(context.WithoutCancel(ctx), msg, "siren"); err != nil {
				logger.Warn().Err(err).Msg("Crash notification failed")
			}
		})
	}

	// F-15: the SD card's health (read-only / full / I/O errors) — a sticky
	// problem, a Pushover at most hourly, on the diagnostics and the
	// terminals' status. Repos report through dbhealth.Note.
	dbMonitor := dbhealth.New(notifier.Send, meters.DBErrors, logger)
	safego.Go("dbhealth", func() { dbMonitor.Run(ctx, db, 5*time.Minute) })

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
	// re-wiring the printer service. The receipt settings come from the order
	// service's in-memory copy (receiptPrefs is pointed at it once the service
	// exists, before anything prints).
	receiptPrefs := order.DefaultReceiptPrefs
	receiptBuilder := func(snap order.Snapshot, t domainprinter.Ticket) []byte {
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		return receipt.Build(snap, sc, t, receiptPrefs())
	}

	dispatcher := printerApp.NewDispatcher(ctx, factory, printerRegistry, notifier, cfg.PrinterDetectDelay, receiptBuilder, logger, meters)
	// L-5: a printer counts as down only after failing this long without a
	// break: location setting printer_down_after_seconds (main's Store
	// pages; default 30, 10–600).
	dispatcher.SetDownAfter(func() time.Duration {
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		secs := sc.LocationConfigInt("printer_down_after_seconds", int(printerApp.DefaultDownAfter/time.Second))
		return time.Duration(min(max(secs, 10), 600)) * time.Second
	})
	dispatcher.SetStatusHook(func(up bool, msg string) {
		if up {
			devRing.Add(devlog.Info, devlog.KindPrinter, msg)
		} else {
			devRing.Add(devlog.Warn, devlog.KindPrinter, msg)
		}
	})

	// Printers on the LAN: learn each roster printer's MAC after a good probe,
	// follow one that moved to a new IP, list unknown ones for "add". The
	// probe hook must be set before the first roster Apply (hydrate below);
	// the RPC side is wired once the registry exists.
	printerWatcher := printernet.New(printernet.Deps{
		Store:  repositories.NewPrinterNetRepository(db),
		Roster: dispatcher.RosterPrinters,
		Status: printerRegistry.All,
		Notify: notifier.Send,
	}, logger)
	printerWatcher.Load(ctx)
	printerRegistry.SetMACLookup(printerWatcher.MACByName)
	dispatcher.SetProbeHook(printerWatcher.OnProbe)

	// Store map (PMTiles) for the tablets' delivery maps: downloaded next to
	// the DB when store_config's map version changes, served at
	// /api/map.pmtiles.
	mapFile := mapfile.New(filepath.Join(filepath.Dir(cfg.AgentDBPath), mapfile.FileName), orderRepo, logger)
	mapFile.Load(ctx)

	if saved, err := storeConfigRepo.Get(ctx); err == nil && saved != nil {
		storeCtx.Store(*saved)
		notifier.SetOwnerDevices(ownerDevices(*saved))
		dispatcher.Apply(ctx, saved.Printers)
		mapFile.Want(saved.Location.Map)
		logger.Info().Ctx(ctx).
			Str("tenant", saved.Tenant.Name).
			Str("location", saved.Location.Name).
			Int("printers", len(saved.Printers)).
			Msg("store_config hydrated from DB")
	}

	// printFn plans the order's tickets for a mode (accept: stations + receipt;
	// reprint: receipt only; kitchen resend: stations only) and prints them.
	printFn := dispatcher.ExecuteMode

	// --- Order service + mobile-facing SSE broker -------------------------
	broker := ssebroker.New()
	orderService := orderApp.NewService(orderRepo, settingsRepo, printFn, notifier, broker, logger, meters)
	orderService.SetDevLog(devRing)
	// §2.10aa: every order from main is placed on arrival. AGENT_TEST_ACCEPT_STEP=1 (dev only, the
	// older pytest suites of the kept accept path) lets arrivals wait for the auto-accept setting.
	if os.Getenv("AGENT_TEST_ACCEPT_STEP") == "1" {
		if cfg.AppEnv == "dev" {
			orderService.KeepAcceptStep(true)
			logger.Warn().Msg("AGENT_TEST_ACCEPT_STEP: arrivals wait for an accept (dev tests only)")
		} else {
			logger.Warn().Str("app_env", cfg.AppEnv).Msg("AGENT_TEST_ACCEPT_STEP refused outside dev: orders are placed on arrival")
		}
	}
	// F-7: auto-accepts (and their prints) run on a worker, off the SSE reader.
	orderService.StartAcceptWorker(ctx)
	// Driver reports print on the receipt printer (failover like a ticket),
	// formatted with the live store context (timezone + store name).
	// Each slip is laid out for the printer it lands on (on.Cols).
	orderService.SetDocumentPrinting(dispatcher.PrintDocument, func(rep order.DriverReport, on domainprinter.Paper) []byte {
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		return receipt.BuildDriverReport(rep, sc, orderService.PrintPrefs().DriverReportSize, on, time.Now())
	})
	orderService.SetUnassignedReport(func(rep order.UnassignedReport, on domainprinter.Paper) []byte {
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		return receipt.BuildUnassignedReport(rep, sc, orderService.PrintPrefs().DriverReportSize, on, time.Now())
	})
	orderService.SetUnsettledReport(func(l order.UnsettledList, on domainprinter.Paper) []byte { // contract 2.10s §4
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		return receipt.BuildUnsettledList(l, sc, orderService.PrintPrefs().DriverReportSize, on, time.Now())
	})

	orderService.LoadReceiptPrefs(ctx)
	receiptPrefs = orderService.PrintPrefs
	// 2.1.0: feature toggles, kitchen-ticket switches, default times and the
	// store hub's last status; the print path reads the switches + whether
	// the hub inserts online cash pickups live, per job.
	orderService.LoadIntegrationSettings(ctx)
	dispatcher.SetRules(func() printerApp.Rules {
		return printerApp.Rules{Tickets: orderService.IntegrationSettings().Tickets}
	})
	// Routing & emergency reroute (contract 2.3c): a Pi setting, read per
	// ticket; a save wakes state sync (main keeps it in agent.state for a
	// re-imaged Pi). Loaded before anything can print.
	var wakeStateSync func() // set once state sync exists, before HTTP starts
	dispatcher.SetRoutingStore(orderRepo, func() {
		if wakeStateSync != nil {
			wakeStateSync()
		}
	})
	dispatcher.LoadRouting(ctx)
	// POS orders are made on this Pi: they carry the linked tenant + location.
	orderService.SetStoreIDs(func() (string, string) {
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		return sc.Tenant.ID, sc.Location.ID
	})

	if n := orderService.RecoverInterruptedAccepts(ctx); n > 0 {
		logger.Warn().Ctx(ctx).Int("orders", n).Msg("Orders whose accept was interrupted will ring again")
	}

	// --- Store_config SSE handshake sink ----------------------------------
	sink := &storeConfigApplier{
		notifier:   notifier,
		repo:       storeConfigRepo,
		dispatcher: dispatcher,
		mapFile:    mapFile,
		onApplied:  broker.PublishFloorChanged,
		storeCtx:   &storeCtx,
		logger:     logger.With().Str("module", "storeconfig-sink").Logger(),
		telemetry:  cfg.OTELEndpoint != "",
		resTenant:  resTenant,
		resLoc:     resLocation,
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
		return printerApp.BuildReport(printerRegistry, version, printerWatcher.Unknown()), nil
	})

	// Where main located a delivery address (+ drive times for AUTO).
	registry.Register(agentrpc.MsgOrderGeo, orderService.HandleGeoCommand)

	sseCtrl := ssecontroller.New(ctx, cfg.MainServerURL, orderService, sink, registry, logger, meters)
	rpcPoster.key = sseCtrl.CurrentKey

	// POS terminals (terminal stage 0d): the counter menu + terminal config
	// snapshots main sends (kept in SQLite, refetched at connect and on
	// main's menu.changed / terminal.changed), and the /api/terminal/* flow.
	termStore := terminalApp.NewStore(orderRepo, func(rc context.Context, path string) (int, []byte, error) {
		k := sseCtrl.CurrentKey()
		if k == "" {
			return 0, nil, fmt.Errorf("not linked to a store")
		}
		return mainClient.Download(rc, k, path, "")
	}, broker, logger)
	termSvc := terminalApp.NewService(termStore, orderRepo, orderService, broker, logger)
	termSvc.SetFloor(orderService, orderRepo) // the driver view (§2.10b)
	termSvc.SetPager(notifier)                // an unbalanced Z, PIN lockouts (hardening M-9 / X-2)
	termSvc.SetPinRepo(ctx, orderRepo)        // the PIN throttle survives a restart (X-2)
	// Close batch closes the drawers and queues the Z report (§2.10k-l):
	// built right after, and at start for one a crash left pending.
	termSvc.SetZDrivers(orderService.DriverReports)
	termSvc.SetReports(orderService) // the Manager tab (§2.10m)
	termSvc.SetStoreName(func() string { sc, _ := storeCtx.Load().(storeconfig.StoreContext); return sc.StoreName() })
	termSvc.SetStoreInfo(func() map[string]any { // the terminals' receipt header / footer (contract §2.10q)
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		return terminalApp.StoreInfo(sc, orderService.PrintPrefs())
	})
	orderService.OnClosed(termSvc.AfterBatchClose)
	termSvc.SetDispatch(orderService) // the terminal's Dispatch / Pickup bars (contract §2.10v)
	safego.Go("terminal.pending_z", func() { termSvc.BuildPendingZ(ctx) })
	// M-11: counter orders a restart cut off between commit and print.
	safego.Go("terminal.kitchen_pending", func() { termSvc.ResumeKitchenPrints(ctx) })
	termStore.Load(ctx)
	orderService.SetArrivalStamp(termStore.StampDriverTicket) // show_on_driver_ticket on channel orders
	registry.Register(terminalApp.MsgMenuChanged, termStore.HandleMenuChanged)
	registry.Register(terminalApp.MsgTerminalChanged, termStore.HandleTerminalChanged)
	termStore.Run(ctx, 5*time.Minute, sseCtrl.Configured)
	// The store's customers (terminal 1d): a copy of main's, pulled from its
	// change feed at connect, on main's customers.changed and every 3 min.
	custBook := terminalApp.NewCustomerBook(orderRepo, func(rc context.Context, path, rawQuery string) (int, []byte, error) {
		k := sseCtrl.CurrentKey()
		if k == "" {
			return 0, nil, fmt.Errorf("not linked to a store")
		}
		return mainClient.Download(rc, k, path, rawQuery)
	}, broker, func() string {
		if !sseCtrl.Configured() {
			return ""
		}
		_, loc := orderService.StoreIDs()
		return loc
	}, func() (*float64, *float64, int64) {
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		return sc.Location.Lat, sc.Location.Lng, sc.Location.DeliveryRadiusM
	}, logger)
	termSvc.SetCustomers(custBook)
	registry.Register(terminalApp.MsgCustomersChanged, custBook.HandleChanged)
	orderService.OnCustomerOrder(func(rc context.Context, phone string, at time.Time) { custBook.NoteOrder(rc, "", phone, at) })
	safego.Go("terminal.customers", func() { custBook.Run(ctx, 3*time.Minute, sseCtrl.Configured) })
	// Delivery maps + AUTO: the store pin and map for /api/floor, and the
	// agent->main geo RPCs (pin, trip).
	orderService.SetGeo(orderApp.GeoDeps{
		Floor: func() (*orderApp.FloorStore, *orderApp.FloorMap) {
			sc, _ := storeCtx.Load().(storeconfig.StoreContext)
			var store *orderApp.FloorStore
			if sc.Location.Lat != nil && sc.Location.Lng != nil {
				store = &orderApp.FloorStore{Lat: *sc.Location.Lat, Lng: *sc.Location.Lng, DeliveryRadiusM: sc.Location.DeliveryRadiusM}
			}
			var mp *orderApp.FloorMap
			if info, _ := mapFile.Current(); info != nil {
				bbox := info.BBox
				if bbox == nil {
					bbox = []float64{}
				}
				mp = &orderApp.FloorMap{Version: info.Version, URL: "/api/map.pmtiles", BBox: bbox}
			}
			return store, mp
		},
		Send:   registry.Send,
		Linked: sseCtrl.Configured,
		// The location's delivery radius (store_config) — the tablet changes
		// it through main (location.radius); the in-memory copy is patched
		// until main's store_config push lands.
		Radius: func() (int64, bool) {
			sc, _ := storeCtx.Load().(storeconfig.StoreContext)
			return sc.Location.DeliveryRadiusM, sc.Location.ID != ""
		},
		SetRadius: func(m int64) {
			sc, _ := storeCtx.Load().(storeconfig.StoreContext)
			sc.Location.DeliveryRadiusM = m
			storeCtx.Store(sc)
		},
	})
	mapFile.OnChange(broker.PublishFloorChanged)
	safego.Go("mapfile", func() { mapFile.Run(ctx) })
	printerWatcher.SetMain(registry.Send, func() bool { return sseCtrl.Status().Connected })

	// Pi updates: gate + drop update.request for the root updater, follow
	// update.status, report agent.update.status, Pushover the developer.
	exe, _ := os.Executable()
	updateSupported := update.Supported(cfg.AppEnv, cfg.UpdateDir, cfg.UpdateEnabled)
	if updateSupported {
		initUpdateRuns(ctx, meters)
	}
	manifestEnv := update.ManifestEnv(cfg.AppEnv)
	if manifestEnv == "" && cfg.UtilityURL != "" {
		manifestEnv = "staging" // tests point UTILITY_URL at a fake
	}
	updater := update.New(update.Config{
		Dir:       cfg.UpdateDir,
		Supported: updateSupported,
		Version:   version,
		Commit:    commit,
		Env:       cfg.AppEnv,
		StoreName: func() string { sc, _ := storeCtx.Load().(storeconfig.StoreContext); return sc.StoreName() },
		OnFinished: func(_ context.Context, s update.Status) {
			countUpdateRun(meters, procStart, s)
		},
		OnEvent: func(level, text string) { devRing.Add(level, devlog.KindUpdate, text) },
	}, orderRepo.CountOpenBatchOrders, settingsRepo, registry.Report, notifier.Send,
		update.NewLatest(update.ManifestURL(cfg.UtilityURL, manifestEnv), exe), logger)
	updater.Load(ctx)
	updateCmd := func(action string) agentrpc.CommandHandler {
		return func(rc context.Context, payload json.RawMessage) (any, error) {
			var p struct {
				ID          string `json:"id"`
				Force       bool   `json:"force"`
				RequestedBy string `json:"requested_by"`
			}
			_ = json.Unmarshal(payload, &p)
			// Replies at once: the root updater does the work, the status
			// loop reports its progress (agent.update.status).
			return updater.Request(rc, action, p.Force, p.RequestedBy, p.ID), nil
		}
	}
	registry.Register(update.MsgUpdate, updateCmd(update.ActionUpdate))
	registry.Register(update.MsgRollback, updateCmd(update.ActionRollback))
	safego.Go("update", func() { updater.Run(ctx) })
	safego.Go("printernet", func() { printerWatcher.Run(ctx) })
	// Pi auto-update (2.2.0): after a successful Close batch (its driver
	// reports printed), a newer agent in the manifest is installed through
	// the normal update path (requested_by auto) when auto_update is on.
	var termAppUpdater *appupdate.Updater // set below (the terminal's APK cache)
	orderService.OnBatchClosed(func(rc context.Context) {
		// A good moment for the terminals to install a cached APK (§2.10w).
		if termAppUpdater != nil {
			if rel := termAppUpdater.Current(); rel != nil {
				termSvc.AnnounceApp(terminalApp.EventAppInstall, *rel, "batch_closed")
			}
		}
		rep, reason := updater.AfterClose(rc)
		switch {
		case rep != nil && rep.Accepted:
			devRing.Add(devlog.Info, devlog.KindUpdate, "Auto-update after Close batch requested ("+rep.ID+")")
		case reason == update.AutoOff, reason == update.CodeNotSupported:
		default:
			devRing.Add(devlog.Info, devlog.KindUpdate, "Auto-update after Close batch: nothing to do ("+reason+")")
		}
	})

	// Delivery guarantee: every stored order (new or a redelivery) is acked
	// to main, which re-sends anything unacked after 10 s.
	orderAcker := acker.New(registry.Report, sseCtrl.Configured, logger)
	orderService.OnStored(orderAcker.Ack)
	safego.Go("acker", func() { orderAcker.Run(ctx) })
	// F-1: an order frame that panicked twice is acked (main stops
	// redelivering it) and paged for manual entry — no customer data.
	sseCtrl.OnPoison(func(rc context.Context, snap order.Snapshot) {
		orderAcker.Ack(snap.ID)
		short := snap.ID
		if len(short) > 8 {
			short = short[len(short)-8:]
		}
		msg := "An online order (…" + short + ") made the Pi fail twice: it was set aside. Find it on main's order list and enter it by hand."
		safego.Go("notify.poison", func() {
			if err := notifier.SendFloor(context.WithoutCancel(rc), msg, "siren"); err != nil {
				logger.Warn().Err(err).Msg("Poison order notification failed")
			}
		})
	})

	// Floor state for a replacement Pi (agent.state) + the heartbeat.
	stateSync := statesync.New(orderRepo.AgentState, registry.Report, sseCtrl.Configured, logger)
	safego.Go("statesync", func() { stateSync.Run(ctx) })
	wakeStateSync = stateSync.Wake
	// The paired tablets (+ Quibase versions) come from the HTTP server,
	// built further down.
	var httpRef atomic.Pointer[transport.Server]
	heartbeat := health.New(health.Inputs{
		Tablets: func(rc context.Context) any {
			if hs := httpRef.Load(); hs != nil {
				return hs.Tablets(rc)
			}
			return nil
		},
		DeviceID: func(rc context.Context) string {
			li, _ := settingsRepo.GetLinkInfo(rc)
			return li.DeviceID
		},
		SSE: func() (bool, int64) {
			st := sseCtrl.Status()
			return st.Connected, st.Reconnects
		},
		AcksPending: orderAcker.Pending,
		FloorBacklog: func(rc context.Context) int64 {
			n, _ := orderRepo.CountUnsyncedEvents(rc)
			return n
		},
		Printers: func() (up, down int) {
			for _, p := range printerRegistry.All() {
				if p.Up {
					up++
				} else {
					down++
				}
			}
			return up, down
		},
		LastBackup: func(rc context.Context) string {
			at, _ := settingsRepo.GetLastBackup(rc)
			return at
		},
		Update: func(rc context.Context) any { return updater.Health(rc) },
	}, registry.Report, sseCtrl.Configured, cfg.AgentDBPath, version, logger)
	safego.Go("health", func() { heartbeat.Run(ctx, 5*time.Minute) })

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
	// F-4 (contract 2.10s §2): a 401 alone never wipes. The guard asks
	// main's whoami and wipes only on device_revoked confirmed twice a minute
	// apart; anything else is retried (main_auth_failing on the Pi page).
	revokeGuard := revoke.New(mainClient.WhoAmI, wipeProvisioning, notifier, logger)
	sseCtrl.OnRejected(func(key string) { revokeGuard.Unauthorized(key, "") })
	// Orders whose SSE frame was lost when the stream dropped: fetch them
	// from main after every reconnect (OnArrival skips ones we already hold).
	catchupHook := catchup.Hook(ctx, func(rc context.Context, path, rawQuery string) (int, []byte, error) {
		return mainClient.Get(rc, sseCtrl.CurrentKey(), path, rawQuery)
	}, orderService, orderRepo, logger)
	// Quibase releases: cached next to the DB, re-checked on every reconnect
	// and every 5 minutes; tablets download them over the LAN.
	appUpdater := appupdate.New(filepath.Join(filepath.Dir(cfg.AgentDBPath), "apk"), orderRepo, mainClient, sseCtrl.CurrentKey, logger)
	appUpdater.Load(ctx)
	safego.Go("appupdate", func() { appUpdater.Run(ctx, 5*time.Minute) })
	// The counter terminal's APK (contract §2.10w): cached the same way,
	// served on /api/terminal/app/*, announced when cached and after Close
	// batch. AGENT_TERMINAL_APP renames main's app (pytest only).
	termAppName := os.Getenv("AGENT_TERMINAL_APP")
	if termAppName == "" {
		termAppName = appupdate.AppTerminal
	}
	termAppUpdater = appupdate.NewFor(termAppName, filepath.Join(filepath.Dir(cfg.AgentDBPath), "apk"),
		repositories.TerminalAppStore{R: orderRepo}, mainClient, sseCtrl.CurrentKey, logger)
	termAppUpdater.OnCached(func(rel appupdate.Release) { termSvc.AnnounceApp(terminalApp.EventAppAvailable, rel, "cached") })
	termAppUpdater.Load(ctx)
	safego.Go("appupdate.terminal", func() { termAppUpdater.Run(ctx, 5*time.Minute) })
	recoverySvc := recovery.New(settingsRepo, orderRepo, mainClient, logger)
	pushoverFromEnv := os.Getenv("PUSHOVER_APP_TOKEN") != "" || os.Getenv("PUSHOVER_USER_KEY") != ""
	recoverySvc.OnSecrets = func(sec order.Secrets) {
		if !pushoverFromEnv {
			notifier.SetKeys(sec.PushoverAppToken, sec.PushoverUserKey, sec.PushoverBossKey)
		}
		if os.Getenv("PUSHOVER_ADMIN_DEVICE") == "" {
			notifier.SetAdminDevices(notify.ParseDevices(sec.PushoverAdminDevices))
		}
	}
	sseCtrl.OnDisconnected(func() {
		devRing.Add(devlog.Warn, devlog.KindSSE, "Lost the connection to main")
	})
	// The floor sync is built further down; its "main is back" signal is wired through this.
	var floorReconnected atomic.Pointer[func()]
	sseCtrl.OnConnected(func() {
		devRing.Add(devlog.Info, devlog.KindSSE, "Connected to main")
		if f := floorReconnected.Load(); f != nil {
			(*f)() // the money events go now, not when the queue's backoff runs out
		}
		revokeGuard.Accepted(sseCtrl.CurrentKey())
		stateSync.Reset()
		heartbeat.Wake()
		appUpdater.Wake()
		termAppUpdater.Wake()
		if _, err := recoverySvc.RefreshManagerHash(ctx); err != nil {
			logger.Debug().Err(err).Msg("manager login refresh failed")
		}
		if _, err := recoverySvc.RefreshSecrets(ctx); err != nil {
			logger.Debug().Err(err).Msg("Pushover secrets refresh failed")
		}
		catchupHook()
		termStore.Kick("") // the terminals' menu + config, fresh at every connect
		custBook.Kick()    // the store's customers too
	})
	// Reconciliation: a quiet catch-up pass every minute while connected.
	go catchup.Reconcile(ctx, time.Minute, func(rc context.Context, path, rawQuery string) (int, []byte, error) {
		return mainClient.Get(rc, sseCtrl.CurrentKey(), path, rawQuery)
	}, orderService, orderRepo, func() bool { return sseCtrl.Status().Connected }, logger)
	mainClient.SetOnUnauthorized(revokeGuard.Unauthorized)

	// … and the agent pushes it proactively: on every store_config apply (the
	// roster just changed) and on a periodic heartbeat.
	reportPrinters := func(rc context.Context) {
		if !sseCtrl.Configured() {
			return
		}
		if err := registry.Report(rc, agentrpc.MsgPrinterReport, printerApp.BuildReport(printerRegistry, version, printerWatcher.Unknown())); err != nil {
			logger.Debug().Ctx(rc).Err(err).Msg("printer.report push failed")
		}
	}
	sink.postApply = reportPrinters
	safego.Go("printer.report", func() {
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
	})

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
	httpServer.AddProblems(termSvc.Problems)     // z_unbalanced, pin_lockout on the Pi page
	httpServer.AddProblems(revokeGuard.Problems) // main_auth_failing (F-4)
	if quickCheck != "" {
		httpServer.AddProblems(func(context.Context) []string { return []string{"db_quick_check"} })
	}
	httpServer.SetAltPort(cfg.HTTPAltPort)
	httpServer.SetTerminal(termSvc)
	// The call center's remote terminal (contract 2.10ab): main relays the
	// web app's calls as terminal.relay (run off the SSE reader: a relayed
	// card payment takes seconds), and asks for the terminals' events.
	registry.RegisterAsync(agentrpc.MsgTerminalRelay, httpServer.HandleTerminalRelay, transport.RelayMaxInFlight, transport.RelayBusy())
	ccEvents := terminalApp.NewEventForwarder(func() (<-chan terminalApp.RelayEvent, func()) { return tapEvents(broker) }, registry.Report, logger)
	registry.Register(agentrpc.MsgTerminalEventsWant, ccEvents.HandleWant)
	httpServer.EnsureCallcenterDevice(ctx) // the store's call-center device, on a linked Pi
	httpServer.SetDevLog(devRing)
	httpServer.SetSettingsLog(repositories.NewSettingsLogRepository(db))
	httpServer.SetSetup(repositories.NewSetupRepository(db)) // admin pairing code + setup results (2.10y)
	// Send to phone (2.10z): cashier phones charge with Square's app.
	phonePay := phoneApp.New(phoneApp.Deps{Repo: repositories.NewPhoneChargeRepository(db), Term: termSvc, Devices: deviceRepo,
		RPC: func() phoneApp.Sender { return registry }, Publish: broker.Publish, AppEnv: cfg.AppEnv, Logger: logger})
	httpServer.SetPhonePay(phonePay)
	httpServer.AddProblems(phonePay.Problems) // phone_charge_unrecorded
	safego.Go("phonepay.sweep", func() { phonePay.Run(ctx) })
	httpRef.Store(httpServer)
	httpServer.SetAppUpdater(appUpdater)
	httpServer.SetTerminalAppUpdater(termAppUpdater, func(rc context.Context) bool {
		total, _, err := orderRepo.CountOpenBatchOrders(rc) // right after Close batch, until the first order
		return err == nil && total == 0
	})
	httpServer.SetMap(mapFile)
	httpServer.SetUpdates(updater, printerWatcher)
	httpServer.SetDiag(transport.DiagDeps{
		Dispatcher:  dispatcher,
		Health:      heartbeat,
		StoreCtx:    func() storeconfig.StoreContext { sc, _ := storeCtx.Load().(storeconfig.StoreContext); return sc },
		RPC:         registry,
		AcksPending: orderAcker.Pending,
		TestSlip: func(printerName, tablet string, at time.Time, cols int) []byte {
			sc, _ := storeCtx.Load().(storeconfig.StoreContext)
			return receipt.BuildTestSlip(printerName, sc, tablet, at, cols)
		},
		WidthTest: func(printerName string, at time.Time) []byte {
			sc, _ := storeCtx.Load().(storeconfig.StoreContext)
			if loc, err := time.LoadLocation(sc.Location.Timezone); sc.Location.Timezone != "" && err == nil {
				at = at.In(loc)
			}
			return receipt.BuildWidthTest(printerName, at)
		},
	})
	// Receipt settings (tablet Settings -> Receipts) + PRINT SAMPLE.
	httpServer.SetReceiptSample(func(p order.ReceiptPrefs, kind string, cols int) []byte {
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		switch kind {
		case "future_kitchen", "future_receipt":
			return receipt.BuildFutureSample(sc, p, time.Now(), kind == "future_kitchen", cols)
		}
		return receipt.BuildSample(sc, p, time.Now(), cols)
	})

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
	// Parked events (contract 2.4e) page the owner, at most once an hour,
	// and nudge the Pi page.
	floorSender.SetPager(notifier)
	reconnected := floorSender.Reconnected
	floorReconnected.Store(&reconnected)
	floorSender.OnParked(func(n int) { broker.Publish("sync.parked", map[string]any{"parked": n}) })
	httpServer.SetFloorSync(orderRepo, floorSender.Wake)
	orderService.OnFloorEvent(func() {
		floorSender.Wake()
		stateSync.Wake()
	})

	storeTZ := func() *time.Location {
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		if l, err := time.LoadLocation(sc.Location.Timezone); err == nil && sc.Location.Timezone != "" {
			return l
		}
		return time.Local
	}
	orderService.SetStoreTZ(storeTZ)
	nightly := backup.New(db, cfg.AgentDBPath, func(rc context.Context, path string, body []byte, ct string) (int, []byte, error) {
		return mainClient.Upload(rc, sseCtrl.CurrentKey(), path, body, ct)
	}, settingsRepo, sseCtrl.Configured, storeTZ, logger)
	// F-7: the copy reads on its own read-only connection, never while
	// orders are in play, never around the auto-close minute.
	nightly.SetQuiet(func(rc context.Context) bool {
		rows, err := orderRepo.CountOpenFloor(rc)
		if err != nil {
			return false
		}
		for _, r := range rows {
			if r.N > 0 && r.State != order.StateScheduled {
				return false
			}
		}
		return true
	})
	nightly.SetAvoid(func() (int, int, bool) {
		tc, _ := termStore.Config()
		return parseHHMM(tc.AutoCloseAt())
	})
	if d := cfg.DevTimers["backup"]; d > 0 {
		nightly.Every = d
	}
	defer nightly.Close()
	safego.Go("backup", func() { nightly.Run(ctx) })

	// Manager-login link (headless SD swap). A Pi that was linked without
	// telemetry restarts once so the OTEL exporters pick up the endpoint.
	restart := atomic.Bool{}
	httpServer.SetRecovery(recoverySvc, func(out *recovery.Outcome) {
		stateSync.Reset()
		orderService.LoadReceiptPrefs(ctx) // a restore brings the old Pi's back
		dispatcher.LoadRouting(ctx)        // and its printer routing
		if os.Getenv("AGENT_SELF_RESTART") == "1" && cfg.OTELEndpoint == "" && out.Secrets.OTLPEndpoint != "" {
			logger.Info().Msg("linked: restarting in 3 s to start telemetry")
			time.Sleep(3 * time.Second)
			// The restarted agent labels its telemetry with the store's slugs
			// from the saved store_config: give the new store's first
			// handshake up to 10 s more and make sure it is on disk (a re-used
			// Pi may still hold the previous store's).
			for i := 0; i < 20; i++ {
				if sc, _ := storeCtx.Load().(storeconfig.StoreContext); sc.Tenant.Slug != "" && (out.TenantSlug == "" || sc.Tenant.Slug == out.TenantSlug) {
					if err := storeConfigRepo.Save(ctx, sc); err != nil {
						logger.Warn().Err(err).Msg("linked: saving store_config before the restart failed")
					}
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			restart.Store(true)
			cancel()
		}
	})

	// Live gauges (read at each metric export): open orders by state/source,
	// tablets on the SSE stream, the store hub's link, the agent version.
	if _, err := observability.RegisterLive(observability.Live{
		FloorOpen: func(rc context.Context) ([]observability.FloorCount, error) {
			return openFloorBySource(rc, db)
		},
		TabletsConnected:   httpServer.TabletsConnected,
		TerminalsConnected: httpServer.TerminalsConnected,
		TabletVersions:     httpServer.TabletVersions,
		Hub: func() (observability.HubLink, bool) {
			now := time.Now()
			v := orderService.IntegrationView(now)
			if v.LastSeenAt == nil {
				return observability.HubLink{}, false // no store hub ever reported
			}
			return observability.HubLink{Up: v.Connected, Age: now.Sub(*v.LastSeenAt)}, true
		},
		Version:           version,
		SSEConnected:      func() bool { return sseCtrl.Status().Connected },
		Started:           procStart,
		HostUptime:        health.HostUptime,
		UpdateLastSuccess: updater.LastSuccess,
	}); err != nil {
		logger.Warn().Err(err).Msg("Live metric gauges not registered")
	}

	resetAccess := httpServer.ResetAccess
	onWipe.Store(&resetAccess)
	httpServer.SetDBHealth(dbMonitor.State)
	safego.Go("http", func() { httpServer.Start(ctx) })
	// LAN discovery: the Quibase setup wizard finds this Pi by UDP broadcast.
	hostname, _ := os.Hostname()
	httpPort, _ := strconv.Atoi(cfg.HTTPPort)
	go discovery.New(cfg.DiscoveryPort, httpPort, version, hostname, func() discovery.State {
		key, _ := settingsRepo.GetStoreAPIKey(context.Background())
		sc, _ := storeCtx.Load().(storeconfig.StoreContext)
		store := ""
		if key != "" {
			store = sc.StoreName()
		}
		return discovery.State{Linked: key != "", LinkAvailable: true, Store: store}
	}, logger).Run(ctx)
	safego.Go("retention", func() {
		retention.RunEvery(ctx, orderRepo, cfg.KeepBatches, cfg.DevTimers["retention"], logger, meters)
	})
	safego.Go("floorsync", func() { floorSender.Run(ctx) })
	safego.Go("escalation", func() { escalation.Run(ctx, orderService, logger) })
	safego.Go("driverreset", func() { driverreset.Run(ctx, orderService, storeTZ, logger) })
	// Batch auto-close at main's terminal setting auto_close_batch_at
	// (default 04:00 store time; off until the Pi has main's terminal config).
	autoCloseTick := time.NewTicker(autoclose.Tick)
	defer autoCloseTick.Stop()
	safego.Go("autoclose", func() {
		autoclose.Run(ctx, orderService, autoclose.Deps{
			Setting: func() string { cfg, _ := termStore.Config(); return cfg.AutoCloseAt() },
			Loc:     storeTZ, Now: time.Now, Ticks: autoCloseTick.C,
		}, logger)
	})

	// Late online orders: terminal.order.late once each (the terminals' Info feed).
	lateTick := time.NewTicker(lateorders.Tick)
	defer lateTick.Stop()
	safego.Go("lateorders", func() { lateorders.New(orderService, broker, logger).Run(ctx, lateTick.C, time.Now) })

	// F-6: tell systemd we're up, then ping its watchdog while the one DB
	// connection answers and the SSE goroutine is alive (no-op without
	// NOTIFY_SOCKET).
	safego.Go("watchdog", func() {
		watchdog.Run(ctx, watchdog.Interval(20*time.Second), []watchdog.Check{
			watchdog.DBCheck(db),
			func(context.Context) error {
				if !sseCtrl.Alive(2 * time.Minute) {
					return fmt.Errorf("the SSE client has been silent for 2 minutes")
				}
				return nil
			},
		}, logger)
	})

	<-ctx.Done()
	shutdown(logger, httpServer, orderService, floorSender)
	logger.Info().Msg("Agent shut down")
	if restart.Load() {
		_ = shutdownOtel(context.Background())
		db.Close()
		os.Exit(75) // systemd Restart=always brings it back with the new settings
	}
}

// shutdown drains in order (hardening F-15), inside the unit's
// TimeoutStopSec=15: the HTTP servers finish their requests (5 s), the
// background workers finish what they hold (8 s: an accept's print, driver
// reports), one last floor flush pushes what is queued (3 s); the deferred
// db.Close runs after.
func shutdown(logger zerolog.Logger, hs *transport.Server, svc *orderApp.Service, floor *floorsync.Sender) {
	wait := func(name string, d time.Duration, done <-chan struct{}) {
		select {
		case <-done:
		case <-time.After(d):
			logger.Warn().Str("step", name).Dur("after", d).Msg("Shutdown step timed out")
		}
	}
	wait("http", transport.ShutdownTimeout+time.Second, hs.Stopped())
	workers := make(chan struct{})
	go func() {
		svc.WaitAccepts()
		svc.WaitBackground()
		close(workers)
	}()
	wait("workers", 8*time.Second, workers)
	fctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := floor.Flush(fctx); err != nil {
		logger.Info().Err(err).Msg("Last floor flush before shutdown didn't finish (kept queued)")
	}
}

// openFloorBySource counts the open batch's in-play orders by state and
// source for floor.orders.open: counter (the terminals, CM-12), web (every
// other channel) or online.
func openFloorBySource(ctx context.Context, db *sql.DB) ([]observability.FloorCount, error) {
	rows, err := db.QueryContext(ctx, `SELECT state,
       CASE WHEN source = 'counter' THEN 'counter'
            WHEN source IN ('online', 'pos', '') THEN 'online' ELSE 'web' END AS src,
       count(*)
FROM orders
WHERE batch_id = (SELECT id FROM batches WHERE closed_at IS NULL ORDER BY id DESC LIMIT 1)
  AND state IN ('arrived', 'scheduled', 'accepted', 'ready', 'on_delivery')
GROUP BY state, src`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []observability.FloorCount
	for rows.Next() {
		var st, src string
		var n int64
		if err := rows.Scan(&st, &src, &n); err != nil {
			return nil, err
		}
		out = append(out, observability.FloorCount{State: floorStateLabel(order.State(st)), Source: src, N: n})
	}
	return out, rows.Err()
}

// parseHHMM reads "HH:MM" (the auto-close setting); ok=false for "" / bad.
func parseHHMM(v string) (int, int, bool) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

// persistedStoreSlugs reads the linked store's tenant + location slugs from
// the saved store_config for the telemetry resource. Both are "" (=
// "unlinked") while the agent has no store key, or before the first
// handshake was saved.
func persistedStoreSlugs(ctx context.Context, db *sql.DB, lg zerolog.Logger) (tenant, location string) {
	key, err := repositories.NewSettingsRepository(db, lg).GetStoreAPIKey(ctx)
	if err != nil || key == "" {
		return "", ""
	}
	sc, err := repositories.NewStoreConfigRepository(db, lg).Get(ctx)
	if err != nil || sc == nil {
		return "", ""
	}
	return sc.Tenant.Slug, sc.Location.Slug
}

// updateRunSeries are agent.update.runs' (action, result) pairs, created at 0
// on a Pi so a run's first increment is a visible change in Prometheus.
var updateRunSeries = [][2]string{
	{update.ActionUpdate, update.StateOK}, {update.ActionUpdate, update.StateUpToDate},
	{update.ActionUpdate, update.StateRolledBack}, {update.ActionUpdate, update.StateFailed},
	{update.ActionRollback, update.StateOK}, {update.ActionRollback, update.StateFailed},
}

// updateRunsGrace: a run that finished right after start (the update's own
// restart) is counted only once the zero series has been exported (15 s
// interval), or Prometheus' increase() would miss the 0 -> 1 step.
const updateRunsGrace = 20 * time.Second

func initUpdateRuns(ctx context.Context, m observability.Meters) {
	if m.UpdateRuns == nil {
		return
	}
	for _, p := range updateRunSeries {
		m.UpdateRuns.Add(ctx, 0, metric.WithAttributes(attribute.String("action", p[0]), attribute.String("result", p[1])))
	}
}

// countUpdateRun adds one finished update / rollback to agent.update.runs.
func countUpdateRun(m observability.Meters, procStart time.Time, s update.Status) {
	if m.UpdateRuns == nil {
		return
	}
	action := s.Action
	if action != update.ActionRollback {
		action = update.ActionUpdate
	}
	result := s.State
	switch result {
	case update.StateOK, update.StateUpToDate, update.StateRolledBack:
	default:
		result = update.StateFailed
	}
	add := func() {
		m.UpdateRuns.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("action", action), attribute.String("result", result)))
	}
	if wait := time.Until(procStart.Add(updateRunsGrace)); wait > 0 {
		time.AfterFunc(wait, add)
		return
	}
	add()
}

// floorStateLabel maps an order state to floor.orders.open's state label.
func floorStateLabel(s order.State) string {
	switch s {
	case order.StateArrived:
		return "new"
	case order.StateAccepted:
		return "cooking"
	case order.StateReady:
		return "ready"
	case order.StateOnDelivery:
		return "out"
	case order.StateScheduled:
		return "scheduled"
	}
	return string(s)
}

// applyLinkedSecrets fills empty config from the secrets a manager-login link
// stored (env wins).
// ownerDevices: the store owner's Pushover devices (location config
// pushover_owner_devices, set by the store login in main).
func ownerDevices(sc storeconfig.StoreContext) []string {
	return notify.ParseDevices(sc.LocationConfigString("pushover_owner_devices", ""))
}

func applyLinkedSecrets(cfg *config.Config, sec order.Secrets) {
	if cfg.OTELEndpoint == "" && sec.OTLPEndpoint != "" {
		cfg.OTELEndpoint = sec.OTLPEndpoint
		if os.Getenv("OTEL_EXPORTER_OTLP_HEADERS") == "" && sec.OTLPHeaders != "" {
			_ = os.Setenv("OTEL_EXPORTER_OTLP_HEADERS", sec.OTLPHeaders)
		}
	}
	if cfg.PushoverAppToken == "" && cfg.PushoverUserKey == "" {
		cfg.PushoverAppToken, cfg.PushoverUserKey, cfg.PushoverBossKey = sec.PushoverAppToken, sec.PushoverUserKey, sec.PushoverBossKey
	}
	if os.Getenv("PUSHOVER_ADMIN_DEVICE") == "" {
		cfg.PushoverAdminDevices = sec.PushoverAdminDevices
	}
}

// rpcReplyPoster sends duplex-channel envelopes to main using the agent's
// current store key (read lazily from the SSE controller, so it always uses
// the live key even after a runtime key swap).
type rpcReplyPoster struct {
	client *mainclient.Client
	key    func() string
}

// ExchangeRPC posts an envelope and hands back main's status + body
// (rpc.Exchanger, for Registry.Send).
func (p *rpcReplyPoster) ExchangeRPC(ctx context.Context, body []byte) (int, []byte, error) {
	k := ""
	if p.key != nil {
		k = p.key()
	}
	if k == "" {
		return 0, nil, fmt.Errorf("no store key configured")
	}
	return p.client.PostRPC(ctx, k, body)
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
	mapFile    *mapfile.Downloader
	// onApplied tells the tablets to refetch /api/floor (store pin, map).
	onApplied func()
	// notifier gets the store owner's Pushover devices (location config).
	notifier *notify.Notifier
	storeCtx *atomic.Value
	logger   zerolog.Logger
	// postApply, if set, runs after the roster is rebuilt (used to push a fresh
	// printer.report up to main).
	postApply func(context.Context)
	// The tenant/location slugs the telemetry resource was built with
	// (telemetry on). A store_config with other slugs (first link without a
	// restart) is logged once: the labels follow on the next restart.
	telemetry       bool
	resTenant       string
	resLoc          string
	relabelNoticeMu sync.Mutex
	relabelNotice   string
}

func (a *storeConfigApplier) Apply(ctx context.Context, sc storeconfig.StoreContext) error {
	a.storeCtx.Store(sc)
	if a.notifier != nil {
		a.notifier.SetOwnerDevices(ownerDevices(sc))
	}
	a.dispatcher.Apply(ctx, sc.Printers)
	if a.mapFile != nil {
		a.mapFile.Want(sc.Location.Map) // downloads only when the version changed
	}
	if a.onApplied != nil {
		a.onApplied()
	}
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
	a.noticeRelabel(ctx, sc)
	return nil
}

// noticeRelabel warns (once per new pair) when the store's slugs differ from
// the ones the running telemetry resource carries.
func (a *storeConfigApplier) noticeRelabel(ctx context.Context, sc storeconfig.StoreContext) {
	if !a.telemetry || sc.Tenant.Slug == "" {
		return
	}
	if sc.Tenant.Slug == a.resTenant && sc.Location.Slug == a.resLoc {
		return
	}
	pair := sc.Tenant.Slug + "/" + sc.Location.Slug
	a.relabelNoticeMu.Lock()
	seen := a.relabelNotice == pair
	a.relabelNotice = pair
	a.relabelNoticeMu.Unlock()
	if seen {
		return
	}
	a.logger.Warn().Ctx(ctx).
		Str("tenant_slug", sc.Tenant.Slug).Str("location_slug", sc.Location.Slug).
		Str("telemetry_tenant", observability.StoreLabel(a.resTenant)).
		Msg("store linked or changed: telemetry keeps its tenant/location labels until the agent restarts")
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

// tapEvents subscribes to the /api/events broker for the call center's
// event forwarding (terminal.events): each event stamped as it is read. The
// channel closes when the broker dropped the tap (too slow) or on cancel.
func tapEvents(b *ssebroker.Broker) (<-chan terminalApp.RelayEvent, func()) {
	in, cancel := b.Subscribe()
	out := make(chan terminalApp.RelayEvent, 128)
	done := make(chan struct{})
	var once sync.Once
	safego.Go("callcenter.events_tap", func() {
		defer close(out)
		for ev := range in {
			select {
			case out <- terminalApp.RelayEvent{Event: ev.Type, Data: ev.Data, At: time.Now().UTC()}:
			case <-done:
				return
			}
		}
	})
	return out, func() {
		once.Do(func() { close(done) })
		cancel()
	}
}
