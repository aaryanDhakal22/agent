package observability

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestRateLimitedErrorHandler(t *testing.T) {
	var buf bytes.Buffer
	h := newRateLimitedErrorHandler(zerolog.New(&buf), time.Minute)
	clock := time.Unix(1_000, 0)
	h.now = func() time.Time { return clock }

	boom := errors.New("connection reset")
	h.Handle(boom) // logged
	for i := 0; i < 99; i++ {
		clock = clock.Add(100 * time.Millisecond)
		h.Handle(boom) // suppressed (inside the window)
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Fatalf("logged %d lines inside one window, want 1:\n%s", n, buf.String())
	}

	clock = clock.Add(time.Minute) // window elapsed
	h.Handle(boom)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines after the window, got %d", len(lines))
	}
	if !strings.Contains(lines[1], `"suppressed_since_last":99`) {
		t.Fatalf("second line should report 99 suppressed: %s", lines[1])
	}
}
