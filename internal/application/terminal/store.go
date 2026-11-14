// Package terminal is the Pi's server side for the store's own POS terminals
// (terminal stage 0d, contract/agent.md §2.10): the snapshots main sends for
// them (store.go), staff PIN login + limits (staff.go), quotes and counter
// orders priced with the Pi's copy of main's pricing engine (service.go,
// build.go) and the action journal.
package terminal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/rs/zerolog"
)

// Main's routes and commands (contract/main.md, the 0d contract).
const (
	PathCounterMenu    = "/store/v1/agent/counter-menu"
	PathTerminalConfig = "/store/v1/agent/terminal-config"
	MsgMenuChanged     = "menu.changed"
	MsgTerminalChanged = "terminal.changed"
)

// SSE events for the terminals (and anyone on /api/events).
const (
	EventMenuChanged   = "terminal.menu.changed"
	EventConfigChanged = "terminal.config.changed"
	EventOrderPlaced   = "terminal.order.placed"
	EventOrderPaid     = "terminal.order.paid"
	EventOrderVoided   = terminal.EventOrderVoided
)

// ErrMainUnsupported: main has no such route yet (404).
var ErrMainUnsupported = errors.New("the server has no terminal route yet")

// Fetcher GETs a main route with the store key (status, body).
type Fetcher func(ctx context.Context, path string) (int, []byte, error)

// Publisher fans an event out on /api/events.
type Publisher interface {
	Publish(typ string, v any)
}

// SnapshotRepo stores the last good snapshots.
type SnapshotRepo interface {
	GetTerminalSnapshot(ctx context.Context, kind string) (*terminal.Snapshot, error)
	PutTerminalSnapshot(ctx context.Context, s terminal.Snapshot) error
}

type menuState struct {
	snap terminal.Snapshot
	menu *terminal.CounterMenu
}

type configState struct {
	snap terminal.Snapshot
	cfg  *terminal.Config
}

// Store keeps the counter menu and the terminal config: in SQLite (so a
// restart or an internet outage serves the same one to both terminals) and
// in memory. It refetches on connect, on main's commands and every 5 min
// while one is missing or the last fetch failed.
type Store struct {
	repo     SnapshotRepo
	fetch    Fetcher
	pub      Publisher
	logger   zerolog.Logger
	menu     atomic.Pointer[menuState]
	cfg      atomic.Pointer[configState]
	onConfig atomic.Pointer[[]func(*terminal.Config)]
	kick     map[string]chan struct{}
	failed   sync.Map // kind -> bool: the last fetch failed
	refresh  sync.Mutex
	// drinkRule / drinks: the drink stamps (drinks.go).
	drinkRule atomic.Pointer[func() order.DrinkRule]
	drinks    atomic.Pointer[drinkCache]
}

// NewStore builds the store; Load reads the saved snapshots.
func NewStore(repo SnapshotRepo, fetch Fetcher, pub Publisher, logger zerolog.Logger) *Store {
	return &Store{repo: repo, fetch: fetch, pub: pub, logger: logger.With().Str("module", "terminal-store").Logger(),
		kick: map[string]chan struct{}{terminal.KindCounterMenu: make(chan struct{}, 1), terminal.KindTerminalConfig: make(chan struct{}, 1)}}
}

// OnConfig adds a hook run when a new terminal config is in use (register
// before Load; the terminal service's sessions, cmd/main.go's Quibase rule).
func (s *Store) OnConfig(fn func(*terminal.Config)) {
	for {
		cur := s.onConfig.Load()
		var next []func(*terminal.Config)
		if cur != nil {
			next = append(next, *cur...)
		}
		next = append(next, fn)
		if s.onConfig.CompareAndSwap(cur, &next) {
			return
		}
	}
}

// Load reads the saved snapshots (startup). A bad saved body is skipped.
func (s *Store) Load(ctx context.Context) {
	for _, kind := range []string{terminal.KindCounterMenu, terminal.KindTerminalConfig} {
		snap, err := s.repo.GetTerminalSnapshot(ctx, kind)
		if err != nil || snap == nil {
			if err != nil {
				s.logger.Warn().Err(err).Str("kind", kind).Msg("load terminal snapshot failed")
			}
			continue
		}
		if err := s.use(*snap); err != nil {
			s.logger.Warn().Err(err).Str("kind", kind).Msg("saved terminal snapshot unreadable")
		}
	}
}

// Menu is the counter menu in use (nil = none yet) with its snapshot.
func (s *Store) Menu() (*terminal.CounterMenu, terminal.Snapshot) {
	if m := s.menu.Load(); m != nil {
		return m.menu, m.snap
	}
	return nil, terminal.Snapshot{}
}

// Config is the terminal config in use (nil = none yet) with its snapshot.
func (s *Store) Config() (*terminal.Config, terminal.Snapshot) {
	if c := s.cfg.Load(); c != nil {
		return c.cfg, c.snap
	}
	return nil, terminal.Snapshot{}
}

// use parses a snapshot and puts it in memory.
func (s *Store) use(snap terminal.Snapshot) error {
	switch snap.Kind {
	case terminal.KindCounterMenu:
		m, err := terminal.ParseCounterMenu(snap.Body)
		if err != nil {
			return err
		}
		s.menu.Store(&menuState{snap: snap, menu: m})
	case terminal.KindTerminalConfig:
		c, err := terminal.ParseConfig(snap.Body)
		if err != nil {
			return err
		}
		s.cfg.Store(&configState{snap: snap, cfg: c})
		if fns := s.onConfig.Load(); fns != nil {
			for _, fn := range *fns {
				if fn != nil {
					fn(c)
				}
			}
		}
	default:
		return fmt.Errorf("unknown snapshot kind %q", snap.Kind)
	}
	return nil
}

func etagOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8])
}

// Refresh fetches one snapshot from main; a new body is saved, used and
// announced (SSE). changed = a different body is in use now.
func (s *Store) Refresh(ctx context.Context, kind string) (changed bool, err error) {
	s.refresh.Lock()
	defer s.refresh.Unlock()
	defer func() { s.failed.Store(kind, err != nil) }()
	path := PathCounterMenu
	if kind == terminal.KindTerminalConfig {
		path = PathTerminalConfig
	}
	status, body, err := s.fetch(ctx, path)
	switch {
	case err != nil:
		return false, err
	case status == 404 || status == 405 || status == 501:
		return false, ErrMainUnsupported
	case status != 200:
		return false, fmt.Errorf("GET %s: HTTP %d", path, status)
	}
	snap := terminal.Snapshot{Kind: kind, ETag: etagOf(body), Body: body, FetchedAt: time.Now()}
	var version int64
	switch kind {
	case terminal.KindCounterMenu:
		m, err := terminal.ParseCounterMenu(body)
		if err != nil {
			return false, err
		}
		version = m.MenuVersion
		if cur := s.menu.Load(); cur != nil && cur.snap.ETag == snap.ETag {
			return false, nil
		}
	default:
		c, err := terminal.ParseConfig(body)
		if err != nil {
			return false, err
		}
		version = c.Version
		if cur := s.cfg.Load(); cur != nil && cur.snap.ETag == snap.ETag {
			return false, nil
		}
	}
	snap.Version = version
	if err := s.repo.PutTerminalSnapshot(ctx, snap); err != nil {
		return false, err
	}
	if err := s.use(snap); err != nil {
		return false, err
	}
	s.logger.Info().Ctx(ctx).Str("kind", kind).Int64("version", version).Str("etag", snap.ETag).Msg("Terminal snapshot updated")
	if s.pub != nil {
		if kind == terminal.KindCounterMenu {
			s.pub.Publish(EventMenuChanged, map[string]any{"menu_version": version, "etag": snap.ETag})
		} else {
			s.pub.Publish(EventConfigChanged, map[string]any{"version": version, "etag": snap.ETag})
		}
	}
	return true, nil
}

// Kick asks for a refetch of a kind (or both with ""), without waiting.
func (s *Store) Kick(kind string) {
	for k, ch := range s.kick {
		if kind == "" || kind == k {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// Run serves the kicks (one fetch at a time per kind) and retries every
// retry while a snapshot is missing or its last fetch failed. linked says
// whether the Pi has a store key.
func (s *Store) Run(ctx context.Context, retry time.Duration, linked func() bool) {
	for kind, ch := range s.kick {
		go func(kind string, ch chan struct{}) {
			t := time.NewTicker(retry)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ch:
				case <-t.C:
					failed, _ := s.failed.Load(kind)
					if !s.missing(kind) && failed != true {
						continue
					}
				}
				if linked != nil && !linked() {
					continue
				}
				if _, err := s.Refresh(ctx, kind); err != nil {
					lvl := s.logger.Warn()
					if errors.Is(err, ErrMainUnsupported) {
						lvl = s.logger.Debug()
					}
					lvl.Err(err).Str("kind", kind).Msg("terminal snapshot fetch failed")
				}
			}
		}(kind, ch)
	}
}

func (s *Store) missing(kind string) bool {
	if kind == terminal.KindCounterMenu {
		return s.menu.Load() == nil
	}
	return s.cfg.Load() == nil
}

// HandleMenuChanged is main's `menu.changed {version}` command: refetch
// unless that version is in use. No reply (fire-and-forget); the fetch
// runs behind.
func (s *Store) HandleMenuChanged(_ context.Context, payload json.RawMessage) (any, error) {
	var p struct {
		Version int64 `json:"version"`
	}
	_ = json.Unmarshal(payload, &p)
	if m := s.menu.Load(); m != nil && p.Version > 0 && m.snap.Version == p.Version {
		return nil, nil // the version in use; main expects no reply
	}
	s.Kick(terminal.KindCounterMenu)
	return nil, nil
}

// HandleTerminalChanged is main's `terminal.changed` command (staff or
// terminal settings changed): refetch the config.
func (s *Store) HandleTerminalChanged(_ context.Context, payload json.RawMessage) (any, error) {
	var p struct {
		Version int64 `json:"version"`
	}
	_ = json.Unmarshal(payload, &p)
	if c := s.cfg.Load(); c != nil && p.Version > 0 && c.snap.Version == p.Version {
		return nil, nil
	}
	s.Kick(terminal.KindTerminalConfig)
	return nil, nil
}
