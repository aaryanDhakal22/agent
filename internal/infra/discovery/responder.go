// Package discovery answers the Quibase tablet's LAN probe so a fresh tablet
// can find the Pi without anyone typing an address.
//
// The tablet broadcasts one UDP datagram to the discovery port (default
// 48080):
//
//	{"q":"quibase-discover","v":1,"nonce":"<16 hex>"}   + spaces to >= 200 bytes
//
// and every agent that hears it replies UNICAST to the sender's address:port
// with
//
//	{"v":1,"svc":"quicc-agent","nonce":"…","port":8080,"linked":true,
//	 "link_available":true,"store":"…","version":"…","host":"…"}
//
// The reply is always smaller than the probe (and the probe has a minimum
// size), so the responder can't be used to amplify a spoofed-source flood;
// replies are also rate-limited (20/s). Unicast replies mean the tablet needs
// no Android multicast lock. Stdlib only.
package discovery

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	// Query is the probe's "q" value.
	Query = "quibase-discover"
	// Service is the reply's "svc" value.
	Service = "quicc-agent"
	// MinProbeBytes: shorter datagrams are dropped (anti-amplification).
	MinProbeBytes = 200
	// maxProbeBytes bounds the read buffer; a probe never needs more.
	maxProbeBytes = 2048
	// RatePerSecond caps replies (token bucket, burst = rate).
	RatePerSecond = 20
)

// State is the live part of the reply (read on every probe).
type State struct {
	Linked        bool
	LinkAvailable bool
	Store         string
}

// Probe is the tablet's datagram.
type Probe struct {
	Q     string `json:"q"`
	V     int    `json:"v"`
	Nonce string `json:"nonce"`
}

// Reply is the agent's answer.
type Reply struct {
	V             int    `json:"v"`
	Svc           string `json:"svc"`
	Nonce         string `json:"nonce"`
	Port          int    `json:"port"`
	Linked        bool   `json:"linked"`
	LinkAvailable bool   `json:"link_available"`
	Store         string `json:"store"`
	Version       string `json:"version"`
	Host          string `json:"host"`
}

// Responder answers discovery probes.
type Responder struct {
	port     int
	httpPort int
	version  string
	host     string
	state    func() State
	logger   zerolog.Logger
	limit    *bucket
}

// New builds a responder for UDP port (0 disables Run); httpPort is what the
// tablet should talk HTTP to.
func New(port, httpPort int, version, host string, state func() State, logger zerolog.Logger) *Responder {
	return &Responder{port: port, httpPort: httpPort, version: version, host: host, state: state,
		logger: logger.With().Str("module", "discovery").Logger(), limit: newBucket(RatePerSecond, time.Now())}
}

// Run listens on udp4 :port until ctx ends. A bind failure (the port is taken,
// e.g. a second dev-roster agent on one host) only logs a warning — the agent
// is still reachable by a typed address.
func (r *Responder) Run(ctx context.Context) {
	if r.port <= 0 {
		r.logger.Info().Msg("LAN discovery disabled (AGENT_DISCOVERY_PORT=0)")
		return
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: r.port})
	if err != nil {
		r.logger.Warn().Err(err).Int("port", r.port).Msg("LAN discovery unavailable (can't bind) — tablets must type the address")
		return
	}
	r.logger.Info().Int("port", r.port).Msg("LAN discovery listening")
	r.Serve(ctx, conn)
}

// Serve answers probes on conn until ctx ends (closes conn).
func (r *Responder) Serve(ctx context.Context, conn *net.UDPConn) {
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	buf := make([]byte, maxProbeBytes)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			r.logger.Debug().Err(err).Msg("discovery read failed")
			continue
		}
		out, ok := r.answer(buf[:n], time.Now())
		if !ok {
			continue
		}
		if _, err := conn.WriteToUDP(out, from); err != nil {
			r.logger.Debug().Err(err).Str("to", from.String()).Msg("discovery reply failed")
		}
	}
}

// answer validates one probe and builds the reply (ok=false: stay silent).
func (r *Responder) answer(probe []byte, now time.Time) ([]byte, bool) {
	if len(probe) < MinProbeBytes {
		return nil, false
	}
	var p Probe
	if err := json.Unmarshal(probe, &p); err != nil || p.Q != Query || p.V != 1 || !validNonce(p.Nonce) {
		return nil, false
	}
	if !r.limit.take(now) {
		return nil, false
	}
	var st State
	if r.state != nil {
		st = r.state()
	}
	reply := Reply{V: 1, Svc: Service, Nonce: p.Nonce, Port: r.httpPort, Linked: st.Linked,
		LinkAvailable: st.LinkAvailable, Store: clip(st.Store, 64), Version: clip(r.version, 32), Host: clip(r.host, 64)}
	out, err := json.Marshal(reply)
	if err != nil {
		return nil, false
	}
	// Never answer with more bytes than were sent: shed the free-text fields.
	if len(out) >= len(probe) {
		reply.Store, reply.Host = "", ""
		if out, err = json.Marshal(reply); err != nil || len(out) >= len(probe) {
			return nil, false
		}
	}
	return out, true
}

func validNonce(s string) bool {
	if len(s) != 16 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// clip cuts s to at most n bytes on a character boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	for len(string(r)) > n {
		r = r[:len(r)-1]
	}
	return string(r)
}

// bucket is a token bucket: rate tokens/s, burst = rate.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newBucket(rate int, now time.Time) *bucket {
	return &bucket{rate: float64(rate), tokens: float64(rate), last: now}
}

func (b *bucket) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(b.rate, b.tokens+el*b.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
