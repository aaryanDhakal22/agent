package observability

import (
	"context"
	"net/http"
	"testing"

	"github.com/rs/zerolog"
)

// R-1: pprof is off without PPROF_ADDR, refuses a non-loopback address, and
// serves on loopback.
func TestPprofLoopbackOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if addr, err := StartPprof(ctx, "", zerolog.Nop()); addr != "" || err != nil {
		t.Fatalf("empty: %q %v", addr, err)
	}
	for _, bad := range []string{"0.0.0.0:0", ":0", "192.168.1.5:6061"} {
		if _, err := StartPprof(ctx, bad, zerolog.Nop()); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	addr, err := StartPprof(ctx, "127.0.0.1:0", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
