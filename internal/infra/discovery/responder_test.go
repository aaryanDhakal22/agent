package discovery

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func probe(nonce string, size int) []byte {
	b, _ := json.Marshal(Probe{Q: Query, V: 1, Nonce: nonce})
	if pad := size - len(b); pad > 0 {
		b = append(b, []byte(strings.Repeat(" ", pad))...)
	}
	return b
}

func newResponder() *Responder {
	return New(0, 8080, "2.0.1", "quiccpos-pi", func() State {
		return State{Linked: true, LinkAvailable: true, Store: "Lombardi's Pizza"}
	}, zerolog.Nop())
}

func TestLoopbackProbeReply(t *testing.T) {
	r := newResponder()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Serve(ctx, conn)

	client, err := net.DialUDP("udp4", nil, conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	p := probe("0123456789abcdef", 256)
	if _, err := client.Write(p); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	if n >= len(p) {
		t.Fatalf("reply (%d bytes) must be smaller than the probe (%d)", n, len(p))
	}
	var rep Reply
	if err := json.Unmarshal(buf[:n], &rep); err != nil {
		t.Fatal(err)
	}
	want := Reply{V: 1, Svc: Service, Nonce: "0123456789abcdef", Port: 8080, Linked: true, LinkAvailable: true,
		Store: "Lombardi's Pizza", Version: "2.0.1", Host: "quiccpos-pi"}
	if rep != want {
		t.Fatalf("reply = %+v", rep)
	}

	// A short probe gets no answer.
	if _, err := client.Write(probe("0123456789abcdef", 0)); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := client.Read(buf); err == nil {
		t.Fatal("a short probe must be dropped")
	}
}

func TestAnswerValidation(t *testing.T) {
	r := newResponder()
	now := time.Now()
	bad := [][]byte{
		probe("0123456789abcdef", 100), // too short
		probe("0123456789ABCDEZ", 256), // not hex
		probe("0123", 256),             // wrong length
		[]byte(strings.Repeat(" ", 256)),
		append([]byte(`{"q":"other","v":1,"nonce":"0123456789abcdef"}`), []byte(strings.Repeat(" ", 200))...),
		append([]byte(`{"q":"quibase-discover","v":2,"nonce":"0123456789abcdef"}`), []byte(strings.Repeat(" ", 200))...),
	}
	for _, b := range bad {
		if _, ok := r.answer(b, now); ok {
			t.Errorf("answered a bad probe: %q", strings.TrimSpace(string(b)))
		}
	}
	out, ok := r.answer(probe("fedcba9876543210", 200), now)
	if !ok || !strings.Contains(string(out), `"nonce":"fedcba9876543210"`) {
		t.Fatalf("nonce must be echoed: %s", out)
	}
}

func TestReplyNeverLargerThanProbe(t *testing.T) {
	r := New(0, 8080, "2.0.1", strings.Repeat("h", 64), func() State {
		return State{Store: strings.Repeat("s", 200)}
	}, zerolog.Nop())
	p := probe("0123456789abcdef", MinProbeBytes)
	out, ok := r.answer(p, time.Now())
	if !ok || len(out) >= len(p) {
		t.Fatalf("ok=%v len=%d probe=%d", ok, len(out), len(p))
	}
	if out, ok := r.answer(probe("0123456789abcdef", 400), time.Now()); !ok || !strings.Contains(string(out), strings.Repeat("s", 64)) {
		t.Fatalf("a big probe gets the (clipped) store name: %s", out)
	}
}

func TestRateLimit(t *testing.T) {
	r := newResponder()
	now := time.Now()
	p := probe("0123456789abcdef", 256)
	answered := 0
	for i := 0; i < 50; i++ {
		if _, ok := r.answer(p, now); ok {
			answered++
		}
	}
	if answered != RatePerSecond {
		t.Fatalf("burst: %d answered, want %d", answered, RatePerSecond)
	}
	if _, ok := r.answer(p, now.Add(100*time.Millisecond)); !ok {
		t.Fatal("tokens refill over time (2 per 100 ms)")
	}
	if _, ok := r.answer(p, now.Add(100*time.Millisecond)); !ok {
		t.Fatal("second refilled token")
	}
	if _, ok := r.answer(p, now.Add(100*time.Millisecond)); ok {
		t.Fatal("only 2 tokens after 100 ms")
	}
}

func TestDisabledAndBindFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	newResponder().Run(ctx) // port 0: returns at once
	taken, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	r := New(taken.LocalAddr().(*net.UDPAddr).Port, 8080, "v", "h", nil, zerolog.Nop())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("a bind failure must return, not block")
	}
}
