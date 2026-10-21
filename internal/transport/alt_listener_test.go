package transport

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestAltListenerServesSameHandlerAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	addr := freeAddr(t)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "same handler") })
	if !startAltListener(ctx, addr, h, zerolog.Nop()) {
		t.Fatal("expected the second port to start")
	}
	resp, err := http.Get("http://" + addr + "/api/version")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "same handler" {
		t.Fatalf("body = %q", body)
	}
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get("http://" + addr + "/"); err != nil {
			return // stopped with the context
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("second port still serving after ctx ended")
}

func TestAltListenerBusyPortIsNotFatal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if startAltListener(context.Background(), ln.Addr().String(), http.NotFoundHandler(), zerolog.Nop()) {
		t.Fatal("expected false for a port that is already taken")
	}
}
