package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/ssebroker"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	defaultListLimit = 50
	maxListLimit     = 200
	// SSE keepalive interval. Must be short enough to survive the proxy idle
	// timeout (nginx default 60s). 25s matches main/'s convention.
	sseKeepalive = 25 * time.Second
)

type Server struct {
	service  *orderApp.Service
	broker   *ssebroker.Broker
	printers *printerApp.Registry
	logger   zerolog.Logger
	port     string
}

func NewServer(svc *orderApp.Service, broker *ssebroker.Broker, printers *printerApp.Registry, port string, logger zerolog.Logger) *Server {
	return &Server{
		service:  svc,
		broker:   broker,
		printers: printers,
		port:     port,
		logger:   logger.With().Str("module", "http-server").Logger(),
	}
}

func (s *Server) Start(ctx context.Context) {
	mux := http.NewServeMux()

	// Order routes
	mux.HandleFunc("GET /api/orders", s.withCORS(s.handleListOrders))
	mux.HandleFunc("GET /api/orders/arrived", s.withCORS(s.handleListArrived))
	mux.HandleFunc("GET /api/orders/{id}", s.withCORS(s.handleGetOrder))
	mux.HandleFunc("POST /api/orders/{id}/accept", s.withCORS(s.handleAccept))
	mux.HandleFunc("POST /api/orders/{id}/reprint", s.withCORS(s.handleReprint))

	// Settings routes — only auto-accept today. Kept under /api/settings so new
	// toggles can join without breaking the shape.
	mux.HandleFunc("GET /api/settings/auto-accept", s.withCORS(s.handleGetAutoAccept))
	mux.HandleFunc("PUT /api/settings/auto-accept", s.withCORS(s.handleSetAutoAccept))

	// Printer status routes — read-only cache served by the in-memory
	// Registry; no TCP probe happens at request time.
	mux.HandleFunc("GET /api/printers", s.withCORS(s.handleListPrinters))
	mux.HandleFunc("GET /api/printers/{name}", s.withCORS(s.handleGetPrinter))

	// SSE stream for mobile.
	mux.HandleFunc("GET /api/events", s.withCORS(s.handleEvents))

	// OPTIONS for CORS preflight on any route.
	mux.HandleFunc("OPTIONS /", s.withCORS(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// otelhttp wraps the whole mux: auto-creates a server span per request,
	// extracts any inbound traceparent header, and attaches a context carrying
	// the span to r.Context() for handlers to use downstream.
	handler := otelhttp.NewHandler(mux, "agent-http",
		otelhttp.WithServerName("quicc-agent"),
	)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", s.port),
		Handler: handler,
	}

	go func() {
		<-ctx.Done()
		s.logger.Info().Msg("HTTP server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	s.logger.Info().Str("addr", srv.Addr).Msg("HTTP server listening")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.logger.Error().Err(err).Msg("HTTP server error")
	}
}

// --- order handlers -------------------------------------------------------

// GET /api/orders?offset=0&limit=50 — paginated history (newest-first).
func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	offset, limit := parsePaging(r)
	orders, err := s.service.ListPage(r.Context(), offset, limit)
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

// POST /api/orders/{id}/accept — mobile accept. DB + print atomic; on printer
// failure the order stays arrived for retry.
func (s *Server) handleAccept(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	so, err := s.service.Accept(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, order.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, err)
		case errors.Is(err, order.ErrAlreadyAccepted):
			s.writeError(w, r, http.StatusConflict, err)
		default:
			// Printer failures also land here — surface them as 502 so mobile
			// can show "printer offline" rather than a generic 500.
			s.writeError(w, r, http.StatusBadGateway, err)
		}
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// POST /api/orders/{id}/reprint — works in any state, does not touch printed_date.
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

// --- helpers --------------------------------------------------------------

func parseID(r *http.Request) (int, error) {
	raw := r.PathValue("id")
	id, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid order id %q", raw)
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

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, err error) {
	s.logger.Warn().Ctx(r.Context()).Err(err).Int("status", status).Str("path", r.URL.Path).Msg("Request failed")
	s.writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}
