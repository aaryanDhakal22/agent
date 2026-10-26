// Package devlog keeps the Pi's recent floor events for the tablet's developer
// mode (GET /api/dev/events): an in-memory ring of the last 500 events — order
// arrived / accepted / printed / ready / out / delivered, print trouble and
// failover, printers up/down, the SSE link to main, floor-sync errors, the
// store hub's link, update runs — plus every warn/error log line (Hook).
//
// No PII: events carry order ids, kitchen numbers, printer names and error
// codes only — never a customer's name, phone or address. Callers build the
// text; the log hook takes only a line's message (never its fields).
package devlog

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
)

// Levels.
const (
	Info  = "info"
	Warn  = "warn"
	Error = "error"
)

// Kinds.
const (
	KindOrder   = "order"
	KindPrint   = "print"
	KindPrinter = "printer"
	KindSSE     = "sse"
	KindSync    = "sync"
	KindHub     = "hub"
	KindUpdate  = "update"
	KindSystem  = "system"
)

const (
	// DefaultSize is how many events the ring keeps.
	DefaultSize = 500
	// MaxPage is the most events one GET returns.
	MaxPage = 200
	// maxText bounds one event's text.
	maxText = 300
	// dedupeWindow: a log line whose message equals an event added this
	// recently (or vice versa) is the same thing said twice.
	dedupeWindow = 5 * time.Second
)

// Event is one entry (GET /api/dev/events).
type Event struct {
	Seq   int64     `json:"seq"`
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	Kind  string    `json:"kind"`
	Text  string    `json:"text"`

	fromLog bool
}

// Ring is a fixed-size, thread-safe event buffer. The zero value is not
// usable; a nil *Ring ignores every Add.
type Ring struct {
	mu   sync.Mutex
	buf  []Event
	head int // index of the oldest event
	n    int
	seq  int64
	now  func() time.Time
}

// New makes a ring of size events (<= 0: DefaultSize).
func New(size int) *Ring {
	if size <= 0 {
		size = DefaultSize
	}
	return &Ring{buf: make([]Event, size), now: time.Now}
}

// Add records an event (level info|warn|error, kind as above).
func (r *Ring) Add(level, kind, text string) {
	r.add(level, kind, text, false)
}

func (r *Ring) add(level, kind, text string, fromLog bool) {
	if r == nil {
		return
	}
	text = clip(strings.TrimSpace(text))
	if text == "" {
		return
	}
	switch level {
	case Info, Warn, Error:
	default:
		level = Info
	}
	if kind == "" {
		kind = KindSystem
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	// The same thing said twice (an event + its log line): keep one, with
	// the event's kind.
	for i := 0; i < r.n && i < 8; i++ {
		e := &r.buf[(r.head+r.n-1-i)%len(r.buf)]
		if now.Sub(e.At) > dedupeWindow {
			break
		}
		if e.Text != text {
			continue
		}
		if fromLog {
			return
		}
		if e.fromLog {
			e.Kind, e.fromLog = kind, false
			return
		}
	}
	r.seq++
	ev := Event{Seq: r.seq, At: now, Level: level, Kind: kind, Text: text, fromLog: fromLog}
	if r.n < len(r.buf) {
		r.buf[(r.head+r.n)%len(r.buf)] = ev
		r.n++
		return
	}
	r.buf[r.head] = ev
	r.head = (r.head + 1) % len(r.buf)
}

// Since returns the events after seq, oldest first, at most max (<= 0 or >
// MaxPage: MaxPage), and the newest seq the ring holds (0 when empty).
func (r *Ring) Since(seq int64, max int) ([]Event, int64) {
	if max <= 0 || max > MaxPage {
		max = MaxPage
	}
	out := []Event{}
	if r == nil {
		return out, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i < r.n && len(out) < max; i++ {
		e := r.buf[(r.head+i)%len(r.buf)]
		if e.Seq > seq {
			e.fromLog = false
			out = append(out, e)
		}
	}
	return out, r.seq
}

// Hook feeds every warn / error log line into the ring (kind system). Only
// the line's message is kept, never its fields (errors, ids, names), so a
// message must not carry customer data — the order code logs those in fields.
func (r *Ring) Hook() zerolog.Hook { return logHook{r} }

type logHook struct{ r *Ring }

func (h logHook) Run(_ *zerolog.Event, level zerolog.Level, msg string) {
	switch {
	case level == zerolog.WarnLevel:
		h.r.add(Warn, classify(msg), msg, true)
	case level >= zerolog.ErrorLevel && level <= zerolog.PanicLevel:
		h.r.add(Error, classify(msg), msg, true)
	}
}

// classify guesses a log line's kind from its message.
func classify(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "floor sync"), strings.Contains(m, "floor event"), strings.Contains(m, "agent.state"):
		return KindSync
	case strings.Contains(m, "sse"), strings.Contains(m, "main server"):
		return KindSSE
	case strings.Contains(m, "pos link"), strings.Contains(m, "store hub"), strings.Contains(m, "integration"):
		return KindHub
	case strings.Contains(m, "update"):
		return KindUpdate
	case strings.Contains(m, "printer"):
		return KindPrinter
	case strings.Contains(m, "print"), strings.Contains(m, "ticket"):
		return KindPrint
	case strings.Contains(m, "order"), strings.Contains(m, "accept"):
		return KindOrder
	}
	return KindSystem
}

func clip(s string) string {
	if len(s) <= maxText {
		return s
	}
	cut := maxText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// Short is the first 8 characters of an id (an order uuid) for event text.
func Short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
