// Package safego runs goroutines that can't take the agent down (hardening
// F-1): a panic is recovered, logged at error with its stack (and the trace
// id when the context carries one) and counted on agent.panics{where}.
//
// Use Go for every background goroutine and Recover / Do at the edges where one
// bad input must not end a long-lived loop (an SSE frame, an RPC handler, an
// HTTP request).
package safego

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	mu     sync.RWMutex
	logger = zerolog.Nop()

	counterOnce sync.Once
	counter     metric.Int64Counter

	total atomic.Int64
)

// SetLogger sets the logger panics are written to (cmd/main.go, once the
// agent's logger exists). Before that they go nowhere but the counter.
func SetLogger(l zerolog.Logger) {
	mu.Lock()
	logger = l.With().Str("module", "safego").Logger()
	mu.Unlock()
}

func log() zerolog.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return logger
}

func panics() metric.Int64Counter {
	counterOnce.Do(func() {
		// The global meter delegates to the provider Setup installs later.
		c, err := otel.Meter("quiccpos/agent").Int64Counter("agent.panics",
			metric.WithDescription("Panics recovered by the agent, by where (the goroutine / handler)"))
		if err == nil {
			counter = c
		}
	})
	return counter
}

// Count is how many panics this process recovered (diagnostics, tests).
func Count() int64 { return total.Load() }

// Go runs fn in a new goroutine that recovers a panic.
func Go(name string, fn func()) {
	go func() {
		defer Recover(name)
		fn()
	}()
}

// Recover is deferred at the top of a goroutine or handler: it swallows a
// panic, logs it with its stack and counts it. Must be called directly by
// defer (recover only works there).
func Recover(name string) {
	if r := recover(); r != nil {
		report(context.Background(), name, r)
	}
}

// RecoverCtx is Recover with the request's context (its trace id goes on the
// log line).
func RecoverCtx(ctx context.Context, name string) {
	if r := recover(); r != nil {
		report(ctx, name, r)
	}
}

// Do runs fn and turns a panic into an error (the stack is logged and
// counted). For a loop that must go on with the next item.
func Do(ctx context.Context, name string, fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			report(ctx, name, r)
			err = fmt.Errorf("panic in %s: %v", name, r)
		}
	}()
	fn()
	return nil
}

func report(ctx context.Context, name string, r any) {
	stack := string(debug.Stack())
	l := log()
	l.Error().Ctx(ctx).Str("where", name).Str("panic", fmt.Sprint(r)).Str("stack", stack).
		Msg("Recovered from a panic")
	if c := panics(); c != nil {
		c.Add(ctx, 1, metric.WithAttributes(attribute.String("where", name)))
	}
	total.Add(1)
}

// CrashKeep is how many crash files SetCrashOutput keeps.
const CrashKeep = 5

// SetCrashOutput sends the runtime's fatal crash report (an unrecovered panic,
// a fatal error like a concurrent map write) to crash-<ts>.txt in dir, keeping
// the newest CrashKeep. The empty file of a clean run is removed at the next
// start. Returns the path (or "" when it couldn't be set).
func SetCrashOutput(dir string, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	PruneCrashFiles(dir, CrashKeep-1)
	path := filepath.Join(dir, "crash-"+now.UTC().Format("20060102T150405Z")+".txt")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	f.Close() // SetCrashOutput dups the descriptor
	return path, nil
}

// CrashFiles lists dir's crash files with content, newest first.
func CrashFiles(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, "crash-*.txt"))
	var out []string
	for _, p := range m {
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			out = append(out, p)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// PruneCrashFiles removes empty crash files (clean runs) and all but the
// newest keep non-empty ones.
func PruneCrashFiles(dir string, keep int) {
	m, _ := filepath.Glob(filepath.Join(dir, "crash-*.txt"))
	var full []string
	for _, p := range m {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st.Size() == 0 {
			os.Remove(p)
			continue
		}
		full = append(full, p)
	}
	sort.Strings(full)
	for len(full) > keep && keep >= 0 {
		os.Remove(full[0])
		full = full[1:]
	}
}

// Short is the first line of a panic value / crash file, for a notification.
func Short(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
