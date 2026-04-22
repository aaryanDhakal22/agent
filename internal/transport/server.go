package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/store"

	"github.com/rs/zerolog"
)

type Server struct {
	store   *store.Store
	service *orderApp.Service
	logger  zerolog.Logger
	port    string
}

func NewServer(st *store.Store, svc *orderApp.Service, port string, logger zerolog.Logger) *Server {
	return &Server{
		store:   st,
		service: svc,
		port:    port,
		logger:  logger.With().Str("module", "http-server").Logger(),
	}
}

func (s *Server) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/orders", s.withCORS(s.handleGetOrders))
	mux.HandleFunc("/api/orders/", s.withCORS(s.handleOrderAction))

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", s.port),
		Handler: mux,
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

// GET /api/orders — returns all orders in the 24h store, newest first.
func (s *Server) handleGetOrders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	entries := s.store.List()
	s.logger.Debug().Int("count", len(entries)).Msg("Serving order list")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// POST /api/orders/{id}/reprint — reprints an order from the store.
func (s *Server) handleOrderAction(w http.ResponseWriter, r *http.Request) {
	// Path: /api/orders/{id}/reprint
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[3] != "reprint" {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.Atoi(parts[2])
	if err != nil {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	s.logger.Info().Int("order_id", id).Msg("Reprint requested")

	entry, ok := s.store.GetByID(id)
	if !ok {
		s.logger.Warn().Int("order_id", id).Msg("Reprint requested for order not in store")
		http.Error(w, "order not found in 24h store", http.StatusNotFound)
		return
	}

	if err := s.service.Handle(entry.Order); err != nil {
		s.logger.Error().Err(err).Int("order_id", id).Msg("Reprint failed")
		http.Error(w, "print failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.logger.Info().Int("order_id", id).Msg("Reprint successful")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "reprinted"})
}

func (s *Server) withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}
