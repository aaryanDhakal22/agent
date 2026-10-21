package transport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// startAltListener serves handler on a second address (HTTP_ALT_PORT — port
// 80 on a Pi, so http://quiccpos-pi works without ":8080") until ctx ends.
// It is best-effort: if the port can't be bound (no CAP_NET_BIND_SERVICE, in
// use) it logs a warning and returns false; the main port is unaffected.
func startAltListener(ctx context.Context, addr string, handler http.Handler, logger zerolog.Logger) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Warn().Err(err).Str("addr", addr).Msg("Second HTTP port not available — serving on the main port only")
		return false
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error().Err(err).Str("addr", addr).Msg("Second HTTP port stopped")
		}
	}()
	logger.Info().Str("addr", addr).Msg("HTTP server also listening")
	return true
}
