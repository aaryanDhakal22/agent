package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"quiccpos/agent/internal/application/appupdate"
	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/application/mapfile"
	orderApp "quiccpos/agent/internal/application/order"
	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/application/printernet"
	"quiccpos/agent/internal/application/recovery"
	"quiccpos/agent/internal/application/sseclient"
	"quiccpos/agent/internal/application/update"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/mainclient"
	"quiccpos/agent/internal/infra/ssebroker"

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

type Server struct {
	service       *orderApp.Service
	broker        *ssebroker.Broker
	printers      *printerApp.Registry
	settings      order.SettingsRepository
	sseController *sseclient.Controller
	mainClient    *mainclient.Client
	session       *sessionManager
	devices       device.Repository
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
	recovery      *recovery.Service
	onLinked      func(*recovery.Outcome)
	linkMu        sync.Mutex
	diag          DiagDeps
	testMu        sync.Mutex // one printer test (or sample) at a time
	receiptSample func(order.ReceiptPrefs) []byte
	mainReach     mainProbe
	started       time.Time
	updates       *update.Requester
	printerNet    *printernet.Watcher
	mapFile       *mapfile.Downloader
	// tabletStreams: open /api/events streams per paired tablet id
	// (floor.tablets.connected counts the tablets, not the streams).
	tabletMu      sync.Mutex
	tabletStreams map[string]int
	// appVersions: the Quibase version each device last reported (2.2.0).
	appVerMu    sync.Mutex
	appVersions map[string]appVersionSeen
	// devLog: the developer-mode event ring (GET /api/dev/events).
	devLog *devlog.Ring
}

// SetAppUpdater wires the tablet-app release cache (/api/app/*).
func (s *Server) SetAppUpdater(u *appupdate.Updater) { s.apps = u }

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
		service:       svc,
		broker:        broker,
		printers:      printers,
		settings:      settings,
		sseController: sseController,
		mainClient:    mainClient,
		session:       session,
		devices:       devices,
		pairing:       &pairingCodes{},
		touched:       map[string]time.Time{},
		tabletStreams: map[string]int{},
		appVersions:   map[string]appVersionSeen{},
		version:       version,
		commit:        commit,
		port:          port,
		started:       time.Now(),
		logger:        logger.With().Str("module", "http-server").Logger(),
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
	handler := otelhttp.NewHandler(http.MaxBytesHandler(traceIDHeader(mux), maxRequestBody), "agent-http",
		otelhttp.WithServerName("quicc-agent"),
	)

	// No WriteTimeout: /api/events is a long-lived SSE stream.
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%s", s.port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		<-ctx.Done()
		s.logger.Info().Msg("HTTP server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	if s.altPort != "" && s.altPort != s.port {
		startAltListener(ctx, ":"+s.altPort, handler, s.logger)
	}

	s.logger.Info().Str("addr", srv.Addr).Msg("HTTP server listening")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.logger.Error().Err(err).Msg("HTTP server error")
	}
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
	if all {
		orders, err = s.service.ListPage(r.Context(), offset, limit)
	} else {
		orders, err = s.service.ListByBatch(r.Context(), batch, offset, limit)
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

// GET /api/orders/arrived — orders awaiting acceptance, oldest-first.
func (s *Server) handleListArrived(w http.ResponseWriter, r *http.Request) {
	orders, err := s.service.ListArrived(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
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
	s.devLog.Add(devlog.Info, devlog.KindOrder, "Auto-accept turned "+onOff(body.AutoAccept)+" ("+caller(r)+")")
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

// GET /api/printers/{name} — single printer status.
func (s *Server) handleGetPrinter(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	snap, ok := s.printers.Get(name)
	if !ok {
		s.writeError(w, r, http.StatusNotFound, fmt.Errorf("unknown printer %q", name))
		return
	}
	s.writeJSON(w, http.StatusOK, snap)
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

	// Write the SSE headers immediately — some clients wait for them before
	// treating the stream as connected.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsubscribe := s.broker.Subscribe()
	defer unsubscribe()
	if d, ok := ctx.Value(deviceCtxKey{}).(*device.Device); ok && d.Kind == device.KindTablet {
		defer s.tabletOnStream(d.ID)()
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
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, string(ev.Data)); err != nil {
				return
			}
			flusher.Flush()
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

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, err error) {
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
		w.Header().Set("Access-Control-Expose-Headers", "X-Trace-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}
