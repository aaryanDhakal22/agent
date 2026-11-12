package escpos

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/printer"

	"github.com/rs/zerolog"
)

// fakePrinter accepts raw-print connections and hands back each job's bytes.
func fakePrinter(t *testing.T) (addr string, jobs <-chan []byte, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan []byte, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b, _ := io.ReadAll(c)
				if len(b) > 0 {
					ch <- b
				}
			}(c)
		}
	}()
	return ln.Addr().String(), ch, func() { ln.Close() }
}

func TestDetectAndPrintOverTCP(t *testing.T) {
	addr, jobs, stop := fakePrinter(t)
	defer stop()
	p := New(addr, "Pizza", zerolog.Nop())
	if err := p.Detect(context.Background()); err != nil {
		t.Fatalf("detect: %v", err)
	}
	want := []byte("\x1b@ticket\x1dVB\x03")
	if err := p.Print(context.Background(), printer.PrintJob{Commands: want}); err != nil {
		t.Fatalf("print: %v", err)
	}
	select {
	case got := <-jobs:
		if !bytes.Equal(got, want) {
			t.Fatalf("printer got %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no job reached the printer")
	}
}

func TestUnreachableAndUnconfigured(t *testing.T) {
	addr, _, stop := fakePrinter(t)
	stop() // nothing listens there now
	p := New(addr, "Wings", zerolog.Nop())
	if err := p.Detect(context.Background()); !errors.Is(err, printer.ErrPrinterUnreachable) {
		t.Fatalf("detect on a dead printer: %v", err)
	}
	if err := p.Print(context.Background(), printer.PrintJob{Commands: []byte("x")}); !errors.Is(err, printer.ErrPrintFailed) {
		t.Fatalf("print on a dead printer: %v", err)
	}
	empty := New("", "Nowhere", zerolog.Nop())
	if err := empty.Detect(context.Background()); !errors.Is(err, printer.ErrPrinterNotConfigured) {
		t.Fatalf("empty ip must not dial (:9100 = localhost): %v", err)
	}
}

func TestSetIPTakesEffect(t *testing.T) {
	a1, jobs1, stop1 := fakePrinter(t)
	defer stop1()
	a2, jobs2, stop2 := fakePrinter(t)
	defer stop2()
	p := New(a1, "Front", zerolog.Nop())
	p.SetIP(a2)
	if err := p.Print(context.Background(), printer.PrintJob{Commands: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-jobs2:
	case <-jobs1:
		t.Fatal("printed to the old address")
	case <-time.After(3 * time.Second):
		t.Fatal("no job")
	}
}

// R-7: one connection at a time per printer — concurrent jobs and probes
// queue instead of opening parallel connections to the raw-print port.
func TestOneConnectionAtATime(t *testing.T) {
	addr, _, stop := fakePrinter(t)
	defer stop()
	var open, peak atomic.Int32
	orig := dial
	dial = func(ctx context.Context, timeout time.Duration, a string) (net.Conn, error) {
		c, err := orig(ctx, timeout, a)
		if err != nil {
			return nil, err
		}
		n := open.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(10 * time.Millisecond) // widen the window
		return &countedConn{Conn: c, open: &open}, nil
	}
	t.Cleanup(func() { dial = orig })
	p := New(addr, "Kitchen", zerolog.Nop())
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = p.Print(context.Background(), printer.PrintJob{Commands: []byte("x")}) }()
		go func() { defer wg.Done(); _ = p.Detect(context.Background()) }()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("peak parallel connections %d, want 1", peak.Load())
	}
}

type countedConn struct {
	net.Conn
	open *atomic.Int32
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.open.Add(-1) })
	return c.Conn.Close()
}
