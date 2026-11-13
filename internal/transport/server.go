package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"quiccpos/agent/internal/application/squareterm"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quiccpos/agent/internal/application/appupdate"
	"quiccpos/agent/internal/application/dbhealth"
	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/application/mapfile"
	orderApp "quiccpos/agent/internal/application/order"
	phoneApp "quiccpos/agent/internal/application/phonepay"
	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/application/printernet"
	"quiccpos/agent/internal/application/recovery"
	"quiccpos/agent/internal/application/sseclient"
	"quiccpos/agent/internal/application/storeupdate"
	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/application/update"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/settingslog"
	"quiccpos/agent/internal/infra/mainclient"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/safego"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultListLimit = 50
	maxListLimit     = 200
	// SSE keepalive interval. Must be short enough to survive the proxy idle
	// timeout (nginx default 60s). 25s matches main/'s convention.
	sseKeepalive = 25 * time.Second
)

// eventsWriteTimeout bounds each /api/events write + flush (R-2; a var so
// tests can shorten it).
var eventsWriteTimeout = 10 * time.Second

type Server struct {
	// storeUpdate runs the store update round (contract §2.10ad; nil = none);
	// writesInFlight counts write requests for its quiet check.
	storeUpdate    *storeupdate.Coordinator
	writesInFlight atomic.Int64
	// problemHooks add GET /api/diag problem codes (AddProblems).
	problemHooks  []func(ctx context.Context) []string
	service       *orderApp.Service
	broker        *ssebroker.Broker
	printers      *printerApp.Registry
	settings      order.SettingsRepository
	sseController *sseclient.Controller
	mainClient    *mainclient.Client
	session       *sessionManager
	devices       device.Repository
	// The call center's relay (server_callcenter.go).
	relayOnce     sync.Once
	relayMux      *http.ServeMux
	ccDevice      callcenterDevice
	pairing       *pairingCodes
	touchMu       sync.Mutex
	touched       map[string]time.Time
	logins        loginThrottle
	version       string
	commit        string
	logger        zerolog.Logger
	port          string
	altPort       string // second port with the same handler (HTTP_ALT_PORT), "" = off
	apps          *appupdate.Updater
	termApps      *appupdate.Updater             // the counter terminal's APK cache (contract 2.10w)
	installWindow func(ctx context.Context) bool // a quiet moment to install: the open batch has no orders
	recovery      *recovery.Service
	onLinked      func(*recovery.Outcome)
	linkMu        sync.Mutex
	diag          DiagDeps
	square        *squareterm.Service // Square on the terminal (2.10r, server_square.go)
	testMu        sync.Mutex          // one printer test (or sample) at a time
	receiptSample func(prefs order.ReceiptPrefs, kind string, cols int) []byte
	mainReach     mainProbe
	started       time.Time
	updates       *update.Requester
	printerNet    *printernet.Watcher
	mapFile       *mapfile.Downloader
	// tabletStreams: open /api/events streams per paired tablet id
	// (floor.tablets.connected counts the tablets, not the streams).
	tabletMu      sync.Mutex
	tabletStreams map[string]int
	// terminalStreams: open /api/events streams per paired terminal id
	// (terminal.connected, CM-2); under tabletMu.
	terminalStreams map[string]int
	// appStates: each terminal's last app-update report (tabletMu; 2.5.3).
	appStates  map[string]TerminalAppState
	onAppState func()
	// stopped is closed once the HTTP server finished its graceful
	// shutdown (Stopped; hardening F-15).
	stopped chan struct{}
	// dbHealth: the SD card's state for the terminals' status route and
	// the diagnostics (F-15; SetDBHealth); nil = unknown.
	dbHealth func() dbhealth.State
	// appVersions: the Quibase version each device last reported (2.2.0).
	appVerMu    sync.Mutex
	appVersions map[string]appVersionSeen
	// devLog: the developer-mode event ring (GET /api/dev/events).
	devLog *devlog.Ring
	// settingsLog: the settings history (2.3.11); auditMu serialises the
	// audited settings PUTs (settings_audit.go).
	settingsLog settingslog.Repository
	auditMu     sync.Mutex
	// floorSync: the parked floor events (contract 2.4e) + the sender's wake.
	floorSync     FloorSyncRepo
	floorSyncWake func()
	// terminalSvc: the POS terminals' API (terminal stage 0d).
	terminalSvc *terminalApp.Service
	phonePay    *phoneApp.Service // Send to phone (2.10z)
	// setups: the admin pairing code + each device's last setup result
	// (contract 2.10y, SetSetup); adminCodes throttles admin-code guesses.
	setups     device.SetupStore
	adminCodes adminCodeThrottle
	// The security round (server_security.go): the environment (a prod Pi
	// refuses the default admin code outside a manager's window) and the
	// admin code's pairing window.
	appEnv  string
	pairWin pairWindow
}

// SetAppUpdater wires the tablet-app release cache (/api/app/*).
func (s *Server) SetAppUpdater(u *appupdate.Updater) { s.apps = u }

// SetTerminalAppUpdater wires the terminal-app release cache
// (/api/terminal/app/*) and the install-window check.
func (s *Server) SetTerminalAppUpdater(u *appupdate.Updater, window func(ctx context.Context) bool) {
	s.termApps, s.installWindow = u, window
}

// SetAltPort makes Start also serve everything on port p (HTTP_ALT_PORT).
func (s *Server) SetAltPort(p string) { s.altPort = p }

func NewServer(
	svc *orderApp.Service,
	broker *ssebroker.Broker,
	printers *printerApp.Registry,
	settings order.SettingsRepository,
	sseController *sseclient.Controller,
	mainClient *mainclient.Client,
	session *sessionManager,
	devices device.Repository,
	port string,
	version string,
	commit string,
	logger zerolog.Logger,
) *Server {
	return &Server{
		service:         svc,
		broker:          broker,
		printers:        printers,
		settings:        settings,
		sseController:   sseController,
		mainClient:      mainClient,
		session:         session,
		devices:         devices,
		pairing:         &pairingCodes{},
		touched:         map[string]time.Time{},
		tabletStreams:   map[string]int{},
		terminalStreams: map[string]int{},
		appVersions:     map[string]appVersionSeen{},
		stopped:         make(chan struct{}),
		version:         version,
		commit:          commit,
		port:            port,
		started:         time.Now(),
		logger:          logger.With().Str("module", "http-server").Logger(),
	}
}

// NewSessionManager exposes session-manager construction to cmd/main.go, which
// resolves the PERSISTED signing secret (load-or-create in the settings row) and
// passes it in so sessions survive restarts.
func NewSessionManager(secret []byte, persist func(hexSecret string) error) *sessionManager {
	m := newSessionManager(secret)
	m.persist = persist
	return m
}

func (s *Server) Start(ctx context.Context) {
	mux := http.NewServeMux()

	// Every route comes from the single-source-of-truth table in routes.go, which
	// also feeds GET /__routes. Add/edit routes there, not here.
	s.register(mux)

	// otelhttp wraps the whole mux: auto-creates a server span per request,
	// extracts any inbound traceparent header, and attaches a context carrying
	// the span to r.Context() for handlers to use downstream. traceIDHeader
	// sits inside otelhttp so the span already exists when it runs.
	// No request needs more than ~256 KB (a tablet's diagnostics report); the
	// open routes decode JSON, so cap every body against a memory blow-up.
	handler := s.Handler(mux)

	// No WriteTimeout: /api/events is a long-lived SSE stream (its writes
	// carry their own deadline, R-2).
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%s", s.port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       idleTimeout,
	}

	// Graceful shutdown (F-15): in-flight requests get ShutdownTimeout;
	// Stopped() closes once that is over, so cmd/main.go stops the workers
	// and closes the DB only after the last request.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		s.logger.Info().Msg("HTTP server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if s.altPort != "" && s.altPort != s.port {
		startAltListener(ctx, ":"+s.altPort, handler, s.logger)
	}

	s.logger.Info().Str("addr", srv.Addr).Msg("HTTP server listening")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.logger.Error().Err(err).Msg("HTTP server error")
	}
	if ctx.Err() != nil {
		<-shutdownDone
	}
	close(s.stopped)
}

// ShutdownTimeout bounds the HTTP servers' graceful shutdown (F-15: the
// systemd unit's TimeoutStopSec=15 covers it, the workers and a floor flush).
const ShutdownTimeout = 5 * time.Second

// idleTimeout closes keep-alive connections nobody uses (R-7), on both ports.
const idleTimeout = 2 * time.Minute

// SetDBHealth wires the SD card monitor (F-15).
func (s *Server) SetDBHealth(f func() dbhealth.State) { s.dbHealth = f }

// Stopped is closed once Start returned (after the graceful shutdown).
func (s *Server) Stopped() <-chan struct{} { return s.stopped }

// Handler is the full HTTP stack around the mux: otelhttp (a server span per
// request, inbound traceparent; it records http.server.request.duration with
// the mux's route, CM-1), panic recovery (F-1), the body cap and the
// X-Trace-Id echo. Exposed for tests.
func (s *Server) Handler(mux http.Handler, opts ...otelhttp.Option) http.Handler {
	return otelhttp.NewHandler(recoverPanics(s.logger, capBodies(traceIDHeader(s.countWrites(mux)))), "agent-http",
		append([]otelhttp.Option{otelhttp.WithServerName("quicc-agent")}, opts...)...,
	)
}

// recoverPanics turns a handler's panic into a 500 (F-1): logged with its
// stack and counted on agent.panics{where="http"}. net/http would recover it
// too, but only to stderr and by dropping the connection.
func recoverPanics(logger zerolog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				safego.Report(r.Context(), "http", v)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal error","code":"internal"}` + "\n"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// traceIDHeader echoes the request's trace id as an X-Trace-Id response
// header, mirroring online/ and main/'s Echo middleware so operators can
// paste any response's trace id straight into Tempo.
func traceIDHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
			w.Header().Set("X-Trace-Id", sc.TraceID().String())
		}
		next.ServeHTTP(w, r)
	})
}

// --- order handlers -------------------------------------------------------

// GET /api/orders?batch=current|all|<id>&offset=0&limit=50 — newest-first;
// defaults to the open batch.
func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	offset, limit := parsePaging(r)
	all, batch, ok := parseBatch(r)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("batch must be current, all or a batch id", "bad_request"))
		return
	}
	var orders []order.StoredOrder
	var err error
	page := func(off, lim int) ([]order.StoredOrder, error) {
		if all {
			return s.service.ListPage(r.Context(), off, lim)
		}
		return s.service.ListByBatch(r.Context(), batch, off, lim)
	}
	if isTablet(r) && s.broker != nil {
		orders, err = s.tabletPage(page, offset, limit)
	} else {
		orders, err = page(offset, limit)
	}
	if orders == nil {
		orders = []order.StoredOrder{}
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, orders)
}

// isTablet: the request comes from a paired Quibase tablet (its device key).
func isTablet(r *http.Request) bool {
	d, ok := r.Context().Value(deviceCtxKey{}).(*device.Device)
	return ok && d.Kind == device.KindTablet
}

// forTablet leaves out, for a paired Quibase tablet, the orders it doesn't
// show (2.5.4: a counter terminal's order at or under the store's X or with
// no kitchen line). The terminals and the Pi page get every order. A page
// may come back shorter than the limit: Quibase pages until an empty one.
func (s *Server) forTablet(r *http.Request, orders []order.StoredOrder) []order.StoredOrder {
	if !isTablet(r) || s.broker == nil {
		return orders
	}
	out := orders[:0:0]
	for _, so := range orders {
		if s.broker.TabletShows(so) {
			out = append(out, so)
		}
	}
	return out
}

// tabletPage is one page of the orders a tablet shows: offset and limit
// count shown orders (Quibase pages by offset until a short page), read from
// the full list in chunks.
func (s *Server) tabletPage(page func(off, lim int) ([]order.StoredOrder, error), offset, limit int) ([]order.StoredOrder, error) {
	const chunk = 500
	out := []order.StoredOrder{}
	skip := offset
	for from := 0; len(out) < limit; from += chunk {
		rows, err := page(from, chunk)
		if err != nil {
			return nil, err
		}
		for _, so := range rows {
			if !s.broker.TabletShows(so) {
				continue
			}
			if skip > 0 {
				skip--
				continue
			}
			if len(out) < limit {
				out = append(out, so)
			}
		}
		if len(rows) < chunk {
			break
		}
	}
	return out, nil
}

// GET /api/orders/arrived — orders awaiting acceptance, oldest-first.
func (s *Server) handleListArrived(w http.ResponseWriter, r *http.Request) {
	orders, err := s.service.ListArrived(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	orders = s.forTablet(r, orders)
	if orders == nil {
		orders = []order.StoredOrder{}
	}
	s.writeJSON(w, http.StatusOK, orders)
}

// GET /api/orders/{id}
func (s *Server) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	so, err := s.service.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, order.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, err)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// POST /api/orders/{id}/accept {prep_minutes?} — claim (kitchen number +
// prep estimate), print, revert if nothing printed (502).
func (s *Server) handleAccept(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	var req struct {
		PrepMinutes *int `json:"prep_minutes"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	so, err := s.service.Accept(r.Context(), id, req.PrepMinutes)
	if err != nil {
		if errors.Is(err, orderApp.ErrNothingPrinted) {
			// The printers are the problem — the tablet shows "printer problem,
			// NOT accepted" and staff retry.
			s.writeJSON(w, http.StatusBadGateway, errCodeMap(err.Error(), "nothing_printed"))
			return
		}
		s.floorErr(w, r, err) // 400 / 404 / 409 already_accepted / 500
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// POST /api/orders/{id}/reprint — the customer receipt only; works in any
// state, does not touch printed_date.
func (s *Server) handleReprint(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	so, err := s.service.Reprint(r.Context(), id)
	if err != nil {
		if errors.Is(err, order.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, err)
			return
		}
		s.writeError(w, r, http.StatusBadGateway, err)
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// reroutedDTO is one station ticket that printed on a fallback printer.
type reroutedDTO struct {
	Station   string `json:"station"`
	PrintedOn string `json:"printed_on"`
}

// POST /api/orders/{id}/kitchen — "Resend to kitchen": print the order's
// station tickets again (no receipt, ignores the pickup-cash rule; no state
// change). 200 {order, printed: [station], failed: [station], rerouted:
// [{station, printed_on}]} — printed lists every station whose ticket printed
// somewhere (rerouted ones included); 409 not_accepted (no kitchen number
// yet) / nothing_for_kitchen (no station tickets); 502 print_failed; 404.
func (s *Server) handleResendKitchen(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	so, out, err := s.service.ResendKitchen(r.Context(), id)
	switch {
	case err == nil:
	case errors.Is(err, order.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, errCodeMap(err.Error(), "not_found"))
		return
	case errors.Is(err, orderApp.ErrNotAccepted):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "not_accepted"))
		return
	case errors.Is(err, orderApp.ErrNothingForKitchen):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "nothing_for_kitchen"))
		return
	case errors.Is(err, orderApp.ErrKitchenNotPrinted):
		s.writeJSON(w, http.StatusBadGateway, errCodeMap(err.Error(), "print_failed"))
		return
	default:
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	printed, failed, rerouted := []string{}, []string{}, []reroutedDTO{}
	for _, p := range out.Printed {
		printed = append(printed, p.Station)
		if p.ReroutedFrom != "" {
			rerouted = append(rerouted, reroutedDTO{Station: p.Station, PrintedOn: p.PrintedOn})
		}
	}
	for _, f := range out.Failed {
		failed = append(failed, f.Station)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"order": so, "printed": printed, "failed": failed, "rerouted": rerouted})
}

// --- settings -------------------------------------------------------------

type autoAcceptDTO struct {
	AutoAccept bool `json:"auto_accept"`
}

func (s *Server) handleGetAutoAccept(w http.ResponseWriter, r *http.Request) {
	v, err := s.service.GetAutoAccept(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, autoAcceptDTO{AutoAccept: v})
}

func (s *Server) handleSetAutoAccept(w http.ResponseWriter, r *http.Request) {
	var body autoAcceptDTO
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, r, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if err := s.service.SetAutoAccept(r.Context(), body.AutoAccept); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, body)
}

// --- version --------------------------------------------------------------

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{
		"service": "quicc-agent",
		"version": s.version,
		"commit":  s.commit,
	})
}

// --- printer status -------------------------------------------------------

// GET /api/printers — snapshot of every registered printer's last probe.
func (s *Server) handleListPrinters(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.printers.All())
}

// --- SSE to mobile --------------------------------------------------------

// GET /api/events — mobile subscribes here. Pushes `order.arrived` events
// broadcast by the order service. Sends an SSE comment every `sseKeepalive` so
// the connection survives proxy idle timeouts.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx proxy buffering

	// Subscribe BEFORE the headers go out: a client treats the stream as
	// connected on the headers and refetches the floor then, so an event
	// published between its refetch and a later Subscribe would be lost to it
	// (§2.10aa: a placed online order reaches Quibase only as order.updated,
	// no ring and no re-announce). Subscribed first, it waits in the channel.
	// A paired Quibase tablet gets only the orders it shows (2.5.4,
	// order.StoredOrder.ShownOnTablet); everyone else gets every order.
	subscribe := s.broker.Subscribe
	if isTablet(r) {
		subscribe = s.broker.SubscribeTablet
	}
	ch, unsubscribe := subscribe()
	defer unsubscribe()

	// Write the SSE headers immediately — some clients wait for them before
	// treating the stream as connected.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	if d, ok := ctx.Value(deviceCtxKey{}).(*device.Device); ok {
		switch d.Kind {
		case device.KindTablet:
			defer s.tabletOnStream(d.ID)()
		case device.KindTerminal:
			defer s.terminalOnStream(d.ID)()
		}
	}
	// R-2: every write gets a deadline, so a tablet that sleeps without
	// closing its socket frees this goroutine within eventsWriteTimeout
	// instead of the kernel's ~15 min.
	rc := http.NewResponseController(w)
	write := func(format string, args ...any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(eventsWriteTimeout))
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	s.logger.Info().Ctx(ctx).Int("subscribers", s.broker.Subscribers()).Msg("Mobile SSE client connected")
	defer s.logger.Info().Ctx(ctx).Msg("Mobile SSE client disconnected")

	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if !write(": keepalive\n\n") {
				return
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if !write("event: %s\ndata: %s\n\n", ev.Type, string(ev.Data)) {
				return
			}
		}
	}
}

// tabletOnStream marks a paired tablet as on the SSE stream; the returned
// func takes it off again.
func (s *Server) tabletOnStream(id string) func() {
	s.tabletMu.Lock()
	s.tabletStreams[id]++
	s.tabletMu.Unlock()
	return func() {
		s.tabletMu.Lock()
		if s.tabletStreams[id]--; s.tabletStreams[id] <= 0 {
			delete(s.tabletStreams, id)
		}
		s.tabletMu.Unlock()
	}
}

// terminalOnStream marks a paired terminal as on the SSE stream (CM-2); the
// returned func takes it off again.
func (s *Server) terminalOnStream(id string) func() {
	s.tabletMu.Lock()
	s.terminalStreams[id]++
	s.tabletMu.Unlock()
	return func() {
		s.tabletMu.Lock()
		if s.terminalStreams[id]--; s.terminalStreams[id] <= 0 {
			delete(s.terminalStreams, id)
		}
		s.tabletMu.Unlock()
	}
}

// TerminalsConnected is terminal.connected (CM-2): every paired, unrevoked
// terminal by its name, 1 while it has an /api/events stream, else 0.
func (s *Server) TerminalsConnected(ctx context.Context) map[string]int {
	out := map[string]int{}
	if s.devices == nil {
		return out
	}
	ds, err := s.devices.List(ctx)
	if err != nil {
		return out
	}
	s.tabletMu.Lock()
	defer s.tabletMu.Unlock()
	for _, d := range ds {
		if d.RevokedAt != nil || d.Kind != device.KindTerminal {
			continue
		}
		if s.terminalStreams[d.ID] > 0 {
			out[d.Name] = 1
		} else if _, ok := out[d.Name]; !ok {
			out[d.Name] = 0
		}
	}
	return out
}

// TabletsConnected is how many paired tablets are on /api/events right now
// (the floor.tablets.connected gauge). Web-UI sessions and the store hub's
// integration key aren't counted.
func (s *Server) TabletsConnected() int {
	s.tabletMu.Lock()
	defer s.tabletMu.Unlock()
	return len(s.tabletStreams)
}

// --- helpers --------------------------------------------------------------

func parseID(r *http.Request) (uuid.UUID, error) {
	raw := r.PathValue("id")
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid order id %q", raw)
	}
	return id, nil
}

func parsePaging(r *http.Request) (offset, limit int) {
	offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	return
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.logger.Warn().Err(err).Msg("Failed to encode JSON response")
	}
}

// maxRequestBody caps every request body (see Start).
const maxRequestBody = 1 << 20

// calloutAudioPath is the one route with a bigger body: the store hub's
// callout preview WAV (<= orderApp.MaxPreviewAudio, its handler enforces it).
const calloutAudioPath = "/api/integration/callout-audio/"

// capBodies caps every request body at maxRequestBody (the callout preview
// upload at MaxPreviewAudio + 1, so its handler can answer 413).
func capBodies(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(maxRequestBody)
		if strings.HasPrefix(r.URL.Path, calloutAudioPath) {
			limit = orderApp.MaxPreviewAudio + 1
		}
		r2 := *r
		r2.Body = http.MaxBytesReader(w, r.Body, limit)
		h.ServeHTTP(w, &r2)
		// CM-1: the mux set the route on the copy; otelhttp reads it from
		// the request it handed in (http.route on the request metrics).
		r.Pattern = r2.Pattern
	})
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, err error) {
	dbhealth.Note(r.Context(), err) // a read-only / full SD card shows here first (F-15)
	s.logger.Warn().Ctx(r.Context()).Err(err).Int("status", status).Str("path", r.URL.Path).Msg("Request failed")
	s.writeJSON(w, status, map[string]string{"error": err.Error()})
}

// errCodeMap builds a {error, code} body. The human message is safe to show;
// the stable code lets the web UI branch (invalid_token, password_mismatch, …).
func errCodeMap(msg, code string) map[string]string {
	return map[string]string{"error": msg, "code": code}
}

func (s *Server) withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, "+deviceKeyHeader+", "+quibaseVersionHeader)
		w.Header().Set("Access-Control-Expose-Headers", "X-Trace-Id, "+autoAssignedHeader)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}
