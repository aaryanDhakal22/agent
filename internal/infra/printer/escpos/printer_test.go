package escpos

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
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
