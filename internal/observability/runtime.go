package observability

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
)

// StartRuntimeMetrics reports the Go runtime (hardening R-1): goroutine count,
// memory in use, GC — go.goroutine.count, go.memory.used, … — read at each
// export through the global meter provider (a no-op without OTLP).
func StartRuntimeMetrics() error {
	return runtime.Start(runtime.WithMinimumReadMemStatsInterval(15 * time.Second))
}

// StartPprof serves net/http/pprof on addr (PPROF_ADDR, R-1) until ctx ends:
// off when addr is empty, and only on a loopback address (reach it on a Pi over
// SSH: `ssh -L 6061:127.0.0.1:6061 …`). Returns the address it listens on.
func StartPprof(ctx context.Context, addr string, logger zerolog.Logger) (string, error) {
	if addr == "" {
		return "", nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("PPROF_ADDR %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", errors.New("PPROF_ADDR must be a loopback address (127.0.0.1:port)")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn().Err(err).Msg("pprof server stopped")
		}
	}()
	logger.Info().Str("addr", ln.Addr().String()).Msg("pprof listening (loopback only)")
	return ln.Addr().String(), nil
}
