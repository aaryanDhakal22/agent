package safego

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestGoRecoversAndLogs(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	SetLogger(zerolog.New(&syncWriter{w: &buf, mu: &mu}))
	defer SetLogger(zerolog.Nop())
	before := Count()
	done := make(chan struct{})
	Go("test.loop", func() {
		defer close(done)
		panic("boom")
	})
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for Count() == before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if Count() != before+1 {
		t.Fatalf("count = %d, want %d", Count(), before+1)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	for _, want := range []string{`"where":"test.loop"`, `"panic":"boom"`, `"stack":`, "Recovered from a panic"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log %q lacks %s", out, want)
		}
	}
}

func TestDoTurnsPanicIntoError(t *testing.T) {
	err := Do(context.Background(), "frame", func() { var m map[string]int; m["x"] = 1 })
	if err == nil || !strings.Contains(err.Error(), "panic in frame") {
		t.Fatalf("err = %v", err)
	}
	if err := Do(context.Background(), "frame", func() {}); err != nil {
		t.Fatalf("clean run err = %v", err)
	}
}

func TestPruneCrashFiles(t *testing.T) {
	dir := t.TempDir()
	for i, n := range []string{"20260101T000000Z", "20260102T000000Z", "20260103T000000Z", "20260104T000000Z"} {
		body := "panic\n"
		if i == 3 {
			body = ""
		}
		os.WriteFile(filepath.Join(dir, "crash-"+n+".txt"), []byte(body), 0o600)
	}
	PruneCrashFiles(dir, 2)
	got := CrashFiles(dir)
	if len(got) != 2 || !strings.Contains(got[0], "20260103") || !strings.Contains(got[1], "20260102") {
		t.Fatalf("kept %v", got)
	}
}

type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
