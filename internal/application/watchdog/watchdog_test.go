package watchdog

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func listen(t *testing.T) *net.UnixConn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.sock")
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	return c
}

func read(c *net.UnixConn, d time.Duration) string {
	_ = c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		return ""
	}
	return string(buf[:n])
}

// F-6: READY, then pings while healthy; none while a check fails (a stuck
// DB transaction); STOPPING at the end.
func TestPingsOnlyWhileHealthy(t *testing.T) {
	sock := listen(t)
	var sick atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, 20*time.Millisecond, []Check{func(context.Context) error {
			if sick.Load() {
				return errors.New("db stuck")
			}
			return nil
		}}, zerolog.Nop())
		close(done)
	}()
	if got := read(sock, time.Second); got != "READY=1" {
		t.Fatalf("first = %q", got)
	}
	if got := read(sock, time.Second); got != "WATCHDOG=1" {
		t.Fatalf("ping = %q", got)
	}
	sick.Store(true)
	time.Sleep(40 * time.Millisecond)
	for read(sock, 10*time.Millisecond) != "" { // drain a ping already in flight
	}
	if got := read(sock, 150*time.Millisecond); got != "" {
		t.Fatalf("pinged while sick: %q", got)
	}
	cancel()
	<-done
	if got := read(sock, time.Second); !strings.Contains(got, "STOPPING=1") {
		t.Fatalf("last = %q", got)
	}
}

func TestNoSocketNoOp(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if ok, err := Notify("READY=1"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if Interval(time.Second) != 0 {
		t.Fatal("interval without a socket")
	}
}
