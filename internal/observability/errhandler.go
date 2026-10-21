package observability

import (
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
)

// otelErrorWindow is how often a persistent export failure is re-logged.
const otelErrorWindow = 60 * time.Second

// rateLimitedErrorHandler replaces the OTEL default error handler (which prints
// every failed export to stderr — hundreds of lines per hour when the collector
// is unreachable). It logs the first error immediately, then at most once per
// window with a count of the errors suppressed in between.
//
// It writes through a plain zerolog logger with NO OTLP bridge hook, so a
// failing log exporter can never feed its own errors back into itself.
type rateLimitedErrorHandler struct {
	logger zerolog.Logger
	window time.Duration
	now    func() time.Time

	mu         sync.Mutex
	lastLogged time.Time
	suppressed int
}

func newRateLimitedErrorHandler(lg zerolog.Logger, window time.Duration) *rateLimitedErrorHandler {
	return &rateLimitedErrorHandler{logger: lg, window: window, now: time.Now}
}

func (h *rateLimitedErrorHandler) Handle(err error) {
	h.mu.Lock()
	now := h.now()
	if !h.lastLogged.IsZero() && now.Sub(h.lastLogged) < h.window {
		h.suppressed++
		h.mu.Unlock()
		return
	}
	suppressed := h.suppressed
	h.suppressed = 0
	h.lastLogged = now
	h.mu.Unlock()

	h.logger.Warn().Err(err).Int("suppressed_since_last", suppressed).
		Dur("window", h.window).Msg("otel export error (collector unreachable?) — rate-limited")
}

// installErrorHandler routes OTEL SDK errors through the rate limiter.
func installErrorHandler() {
	lg := zerolog.New(os.Stderr).With().Timestamp().Str("module", "otel").Logger()
	otel.SetErrorHandler(newRateLimitedErrorHandler(lg, otelErrorWindow))
}
