// Package dbhealth notices an SD card in trouble (hardening F-15). A Pi's
// classic failure is ext4 remounting the card read-only; a full card or I/O
// errors come close. Each used to show only as one request's warning. Now any
// SQLite error of those kinds (Note, from the repos' spans and the HTTP error
// path) and a write probe every few minutes set a sticky problem: counted on
// agent.db.errors{kind}, a Warn line, a Pushover at most hourly, shown on the
// Pi's diagnostics and the terminals' GET /api/terminal/status. A good write
// probe clears it.
package dbhealth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/safego"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"modernc.org/sqlite"
)

// Kinds of trouble.
const (
	ReadOnly = "readonly"
	Full     = "full"
	IOErr    = "ioerr"
)

// SQLite primary result codes (the low byte of an extended code).
const (
	codeReadOnly = 8
	codeIOErr    = 10
	codeCorrupt  = 11
	codeFull     = 13
)

// Classify names an error's SD-card trouble, "" when it is none of them.
func Classify(err error) string {
	if err == nil {
		return ""
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case codeReadOnly:
			return ReadOnly
		case codeFull:
			return Full
		case codeIOErr, codeCorrupt:
			return IOErr
		}
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "readonly database"), strings.Contains(msg, "read-only file system"):
		return ReadOnly
	case strings.Contains(msg, "database or disk is full"), strings.Contains(msg, "no space left on device"):
		return Full
	case strings.Contains(msg, "disk i/o error"), strings.Contains(msg, "input/output error"):
		return IOErr
	}
	return ""
}

// Alerter sends the owner's Pushover (notify.Notifier.Send).
type Alerter func(ctx context.Context, msg, sound string) error

// Monitor holds the sticky state.
type Monitor struct {
	mu        sync.Mutex
	kind      string
	since     time.Time
	lastErr   string
	alertedAt time.Time
	alert     Alerter
	counter   metric.Int64Counter
	logger    zerolog.Logger
	now       func() time.Time
	alertGap  time.Duration
}

var global struct {
	mu sync.RWMutex
	m  *Monitor
}

// New builds the process's monitor and makes it the one Note reports to.
func New(alert Alerter, counter metric.Int64Counter, logger zerolog.Logger) *Monitor {
	m := &Monitor{alert: alert, counter: counter, logger: logger.With().Str("module", "dbhealth").Logger(),
		now: time.Now, alertGap: time.Hour}
	global.mu.Lock()
	global.m = m
	global.mu.Unlock()
	return m
}

// Note reports a DB error to the process's monitor (no-op before New, or
// for an error that isn't SD-card trouble). Cheap: safe on every error path.
func Note(ctx context.Context, err error) {
	kind := Classify(err)
	if kind == "" {
		return
	}
	global.mu.RLock()
	m := global.m
	global.mu.RUnlock()
	if m != nil {
		m.Report(ctx, kind, err)
	}
}

// Report records trouble of kind.
func (m *Monitor) Report(ctx context.Context, kind string, err error) {
	if m.counter != nil {
		m.counter.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
	}
	now := m.now()
	m.mu.Lock()
	first := m.kind == ""
	if first || m.kind != kind {
		m.kind, m.since = kind, now
	}
	if err != nil {
		m.lastErr = err.Error()
	}
	page := m.alert != nil && (m.alertedAt.IsZero() || now.Sub(m.alertedAt) >= m.alertGap)
	if page {
		m.alertedAt = now
	}
	m.mu.Unlock()
	if first {
		m.logger.Warn().Ctx(ctx).Err(err).Str("kind", kind).Msg("SD card trouble: the database can't be written")
	}
	if page {
		msg := "The Pi's SD card is in trouble (" + label(kind) + "): orders may not be saved. Restart the Pi; if it stays, swap the SD card."
		safego.Go("dbhealth.alert", func() {
			if aerr := m.alert(context.WithoutCancel(ctx), msg, "siren"); aerr != nil {
				m.logger.Warn().Err(aerr).Msg("SD card alert not sent")
			}
		})
	}
}

func label(kind string) string {
	switch kind {
	case ReadOnly:
		return "read-only"
	case Full:
		return "full"
	}
	return "I/O errors"
}

// Clear ends the trouble (a good write probe).
func (m *Monitor) Clear(ctx context.Context) {
	m.mu.Lock()
	was := m.kind
	m.kind, m.since, m.lastErr = "", time.Time{}, ""
	m.mu.Unlock()
	if was != "" {
		m.logger.Info().Ctx(ctx).Str("was", was).Msg("SD card writes work again")
	}
}

// State is the trouble now ("" = fine) and since when.
type State struct {
	Trouble string     `json:"trouble"`
	Since   *time.Time `json:"since,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// State reports the sticky state.
func (m *Monitor) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.kind == "" {
		return State{}
	}
	since := m.since.UTC()
	return State{Trouble: m.kind, Since: &since, Error: m.lastErr}
}

// Trouble is State().Trouble.
func (m *Monitor) Trouble() string { return m.State().Trouble }

// ProbeQuery writes without changing anything: SQLite rewrites the row (a WAL
// frame), which fails on a read-only or full card.
const ProbeQuery = `UPDATE settings SET id = id WHERE id = 1`

// Probe runs one write probe now.
func (m *Monitor) Probe(ctx context.Context, db *sql.DB) {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := db.ExecContext(pctx, ProbeQuery)
	if err == nil {
		m.Clear(ctx)
		return
	}
	if kind := Classify(err); kind != "" {
		m.Report(ctx, kind, err)
	}
}

// Run probes at start and every interval until ctx ends.
func (m *Monitor) Run(ctx context.Context, db *sql.DB, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		m.Probe(ctx, db)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
