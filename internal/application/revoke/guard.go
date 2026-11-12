// Package revoke decides when a 401 from main means the Pi's link is really
// gone (contract/agent.md §2.10s §2, HARDENING-PLAN F-4). Before this, any
// 401 wiped the store key and revoked every tablet mid-shift: a main
// restored from an older backup, a proxy's 401 or a route bug sent the store
// to the setup screen.
//
// The rule: the Pi wipes only when main says device_revoked, confirmed by
// GET /store/v1/agent/whoami answering the same twice at least ConfirmAfter
// apart. Any other 401 (unknown_key, no code, a proxy page), a 5xx or a
// transport error never wipes: the Pi keeps retrying, raises the sticky
// problem main_auth_failing and pages the owner at most once an hour. A main
// without whoami (404 / 405) falls back to the old rule, but only after 401s
// seen twice ConfirmAfter apart.
package revoke

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// ConfirmAfter: the second device_revoked must come at least this long after
// the first. PageEvery: main_auth_failing pages at most this often.
const (
	ConfirmAfter = 60 * time.Second
	PageEvery    = time.Hour
	askEvery     = 10 * time.Second
)

// Codes main's agent-key middleware answers a 401 with.
const (
	CodeDeviceRevoked = "device_revoked"
	CodeUnknownKey    = "unknown_key"
)

// WhoAmI asks main about a key: the HTTP status and the error body's code
// (status 0 + err = main unreachable).
type WhoAmI func(ctx context.Context, key string) (status int, code string, err error)

// Pager pages the owner.
type Pager interface {
	Send(ctx context.Context, message, sound string) error
}

// Guard is the 401 rule. Unauthorized is safe from any goroutine.
type Guard struct {
	whoami WhoAmI
	wipe   func(key string)
	pager  Pager
	logger zerolog.Logger
	now    func() time.Time
	after  func(d time.Duration, f func())

	mu         sync.Mutex
	key        string    // the key the state is about
	revokedAt  time.Time // first device_revoked (whoami or, old main, the 401 itself)
	failing    time.Time // main keeps refusing the key without a revoke
	pagedAt    time.Time
	askedAt    time.Time
	scheduled  bool
	wiped      bool
	legacyMain bool
}

// New builds the guard: wipe clears the link (the old wipeProvisioning).
func New(whoami WhoAmI, wipe func(key string), pager Pager, logger zerolog.Logger) *Guard {
	return &Guard{whoami: whoami, wipe: wipe, pager: pager, logger: logger.With().Str("module", "revoke").Logger(),
		now: time.Now, after: func(d time.Duration, f func()) { time.AfterFunc(d, f) }}
}

// CodeOf reads a 401 body's {code}.
func CodeOf(body []byte) string {
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &b)
	return b.Code
}

// Unauthorized is called with every 401 main gave a request made with key
// (code = the body's code, "" when unknown).
func (g *Guard) Unauthorized(key, code string) {
	if key == "" {
		return
	}
	g.mu.Lock()
	if g.key != key {
		g.key, g.revokedAt, g.failing, g.wiped, g.legacyMain = key, time.Time{}, time.Time{}, false, false
	}
	if g.wiped || g.now().Sub(g.askedAt) < askEvery {
		g.mu.Unlock()
		return
	}
	g.askedAt = g.now()
	g.mu.Unlock()
	g.check(key, code)
}

// check asks whoami and applies the rule.
func (g *Guard) check(key, code string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, who, err := g.whoami(ctx, key)
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.key != key || g.wiped {
		return
	}
	switch {
	case err == nil && status == http.StatusOK:
		// The key is fine: the 401 was something else (a route bug, a proxy).
		if !g.failing.IsZero() || !g.revokedAt.IsZero() {
			g.logger.Info().Msg("main accepts the Pi's key again")
		}
		g.logger.Warn().Str("code", code).Msg("main answered 401 but whoami says the key is fine: kept")
		g.revokedAt, g.failing = time.Time{}, time.Time{}
	case err == nil && status == http.StatusUnauthorized && who == CodeDeviceRevoked:
		g.confirm(key, now, "whoami says device_revoked")
	case err == nil && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed):
		// A main without whoami: the old rule, but confirmed over time.
		g.legacyMain = true
		g.confirm(key, now, "401 from a main without whoami")
	default:
		// unknown_key (a main restored from an older backup), another code,
		// 5xx, unreachable: never wipe.
		if g.failing.IsZero() {
			g.failing = now
		}
		g.revokedAt = time.Time{}
		g.logger.Warn().Int("status", status).Str("code", who).AnErr("err", err).
			Msg("main refuses the Pi's key without a revoke: kept, retrying (main_auth_failing)")
		if g.pager != nil && (g.pagedAt.IsZero() || now.Sub(g.pagedAt) >= PageEvery) {
			g.pagedAt = now
			p := g.pager
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_ = p.Send(ctx, "main keeps refusing the Pi's key (not a revoke). The Pi keeps its link and orders; check main (restored from an older backup?).", "siren")
			}()
		}
	}
}

// confirm: the first sign starts the clock (and schedules a re-check); a
// sign at least ConfirmAfter later wipes. Called with mu held.
func (g *Guard) confirm(key string, now time.Time, why string) {
	if g.revokedAt.IsZero() {
		g.revokedAt = now
		g.logger.Warn().Str("why", why).Dur("confirm_after", ConfirmAfter).Msg("main says the Pi's key is revoked: confirming before the wipe")
		if !g.scheduled {
			g.scheduled = true
			g.after(ConfirmAfter+time.Second, func() {
				g.mu.Lock()
				g.scheduled, g.askedAt = false, time.Time{}
				g.mu.Unlock()
				g.check(key, "")
			})
		}
		return
	}
	if now.Sub(g.revokedAt) < ConfirmAfter {
		return
	}
	g.wiped = true
	g.logger.Warn().Str("why", why).Msg("main confirmed the Pi's key is revoked: wiping the link")
	go g.wipe(key)
}

// Failing is the sticky problem: main refuses the key without a revoke.
func (g *Guard) Failing() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.failing.IsZero()
}

// Accepted: a request with key succeeded (the SSE stream is up): the
// problem clears and a pending confirmation is dropped.
func (g *Guard) Accepted(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.key == key {
		g.failing, g.revokedAt = time.Time{}, time.Time{}
	}
}

// Problems is the Pi page's codes (main_auth_failing).
func (g *Guard) Problems(context.Context) []string {
	if g.Failing() {
		return []string{"main_auth_failing"}
	}
	return nil
}
