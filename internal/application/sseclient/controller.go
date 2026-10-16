// Package sseclient is a thin lifecycle wrapper around the raw SSE client
// so the agent can swap its STORE_API_KEY at runtime without restarting
// the process. Two callers care about this:
//
//   - cmd/main.go uses it to launch the initial connection at boot, with
//     whatever key is in the DB (DB is the only source — no env fallback).
//   - The settings HTTP handler uses it to apply a freshly-pasted key from
//     the web UI: cancel the in-flight goroutine, swap the key, restart.
package sseclient

import (
	"context"
	"sync"

	orderApp "quiccpos/agent/internal/application/order"
	sseraw "quiccpos/agent/internal/infra/sse"
	"quiccpos/agent/internal/observability"
	agentrpc "quiccpos/agent/internal/rpc"

	"github.com/rs/zerolog"
)

// Controller owns the SSE client goroutine. SetKey is the only operation
// callers need outside of construction.
type Controller struct {
	serverURL string
	svc       *orderApp.Service
	sink      sseraw.StoreConfigSink
	commands  *agentrpc.Registry
	logger    zerolog.Logger
	meters    observability.Meters
	parentCtx context.Context

	state *sseraw.ConnState // shared connection health, survives key swaps

	mu     sync.Mutex
	cancel context.CancelFunc
	key    string
}

func New(
	parentCtx context.Context,
	serverURL string,
	svc *orderApp.Service,
	sink sseraw.StoreConfigSink,
	commands *agentrpc.Registry,
	logger zerolog.Logger,
	meters observability.Meters,
) *Controller {
	return &Controller{
		serverURL: serverURL,
		svc:       svc,
		sink:      sink,
		commands:  commands,
		logger:    logger.With().Str("module", "sse-controller").Logger(),
		meters:    meters,
		parentCtx: parentCtx,
		state:     sseraw.NewConnState(),
	}
}

// OnRejected registers a callback fired once when main definitively rejects the
// current key (401). The agent wires this to wipe local provisioning + clear the
// key, so a revoked device resets to the setup screen.
func (c *Controller) OnRejected(f func(key string)) { c.state.SetOnRejected(f) }

// OnConnected runs f after every successful SSE (re)connect.
func (c *Controller) OnConnected(f func()) { c.state.SetOnConnected(f) }

// Status returns a snapshot of the live SSE connection health (for the web UI's
// green light + /api/session). Not connected when there's no key.
func (c *Controller) Status() sseraw.ConnStatus {
	c.mu.Lock()
	hasKey := c.key != ""
	c.mu.Unlock()
	st := c.state.Snapshot()
	if !hasKey {
		st.Connected = false
		st.AuthRejected = false
	}
	return st
}

// SetKey atomically swaps the in-flight SSE connection's key. Empty string
// shuts the connection down (the agent stays "unconfigured" until a new
// key is provided). No-op if the key is unchanged — use Reconnect to force a
// restart with the same key.
func (c *Controller) SetKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if key == c.key {
		return
	}
	c.key = key
	c.startLocked("key changed")
}

// Reconnect force-restarts the SSE connection with the CURRENT key, even when
// the key is unchanged. Used after a store-config edit is accepted by main/ so
// the agent re-pulls the updated store_config handshake frame (refreshing the
// receipt context + printer roster). No-op if unconfigured (empty key).
func (c *Controller) Reconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.key == "" {
		return
	}
	c.startLocked("reconnect requested")
}

// startLocked cancels any in-flight client and starts a fresh one for the
// current c.key. Caller must hold c.mu.
func (c *Controller) startLocked(reason string) {
	if c.cancel != nil {
		c.logger.Info().Str("reason", reason).Msg("stopping current SSE client")
		c.cancel()
		c.cancel = nil
	}
	if c.key == "" {
		c.logger.Warn().Msg("STORE_API_KEY cleared — agent is now unconfigured; no SSE connection")
		return
	}

	// Fresh attempt: clear the stale connected/rejected verdict so the UI shows
	// "connecting" until this new key's connect resolves.
	c.state.Reset()

	ctx, cancel := context.WithCancel(c.parentCtx)
	c.cancel = cancel
	client := sseraw.New(c.serverURL, c.key, c.svc, c.sink, c.commands, c.state, c.logger, c.meters)
	go client.Start(ctx)
	c.logger.Info().Str("reason", reason).Msg("SSE client (re)started")
}

// CurrentKey returns the currently-active key. Used by tests + the web UI's
// "are we configured?" indicator. The handler endpoint never returns the
// cleartext — only a boolean.
func (c *Controller) CurrentKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.key
}

// Configured is the public version of "do we have a key?". Use this from
// HTTP handlers; don't leak the cleartext.
func (c *Controller) Configured() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.key != ""
}
