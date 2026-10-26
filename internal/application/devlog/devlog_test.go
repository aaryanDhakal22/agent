package devlog

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestRingWrapsAndPages(t *testing.T) {
	r := New(0)
	for i := 1; i <= 650; i++ {
		r.Add(Info, KindOrder, fmt.Sprintf("event %d", i))
	}
	got, last := r.Since(0, 0)
	if last != 650 || len(got) != MaxPage || got[0].Seq != 151 || got[0].Text != "event 151" {
		t.Fatalf("first page: last %d, %d events, first %+v", last, len(got), got[0])
	}
	got, _ = r.Since(got[len(got)-1].Seq, 0)
	if len(got) != MaxPage || got[0].Seq != 351 {
		t.Fatalf("second page from %d", got[0].Seq)
	}
	got, _ = r.Since(600, 10)
	if len(got) != 10 || got[0].Seq != 601 || got[9].Seq != 610 {
		t.Fatalf("since 600 max 10: %+v", got)
	}
	if got, _ := r.Since(650, 0); len(got) != 0 {
		t.Fatalf("nothing new: %+v", got)
	}
	var nilRing *Ring
	nilRing.Add(Info, KindOrder, "ignored")
	if got, last := nilRing.Since(0, 0); len(got) != 0 || last != 0 {
		t.Fatal("nil ring")
	}
}

func TestLogHookWarnErrorOnlyAndDedupe(t *testing.T) {
	r := New(10)
	lg := zerolog.New(&bytes.Buffer{}).Hook(r.Hook())
	lg.Info().Msg("just info")
	lg.Warn().Str("customer", "Erik Doe").Msg("Floor sync failed; will retry")
	lg.Error().Err(fmt.Errorf("dial 10.0.0.9: refused")).Msg("SSE connection failed, will retry")
	got, _ := r.Since(0, 0)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Level != Warn || got[0].Kind != KindSync || got[0].Text != "Floor sync failed; will retry" {
		t.Fatalf("warn: %+v", got[0])
	}
	if got[1].Level != Error || got[1].Kind != KindSSE {
		t.Fatalf("error: %+v", got[1])
	}
	for _, e := range got { // only the message, never the fields
		if bytes.Contains([]byte(e.Text), []byte("Erik")) || bytes.Contains([]byte(e.Text), []byte("10.0.0.9")) {
			t.Fatalf("fields leaked: %+v", e)
		}
	}
	// An event and its own log line are kept once, with the event's kind.
	r.Add(Warn, KindPrinter, "The Pizza Printer is unreachable.")
	lg.Warn().Msg("The Pizza Printer is unreachable.")
	lg.Warn().Msg("The Wings Printer is unreachable.")
	r.Add(Warn, KindPrinter, "The Wings Printer is unreachable.")
	got, _ = r.Since(2, 0)
	if len(got) != 2 || got[0].Kind != KindPrinter || got[1].Kind != KindPrinter {
		t.Fatalf("dedupe: %+v", got)
	}
}

func TestDedupeWindowAndClip(t *testing.T) {
	r := New(10)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.Add(Info, KindOrder, "same")
	now = now.Add(10 * time.Second)
	r.Add(Info, KindOrder, "same") // a real repeat later on
	long := make([]byte, 400)
	for i := range long {
		long[i] = 'x'
	}
	r.Add("bogus", "", string(long))
	got, _ := r.Since(0, 0)
	if len(got) != 3 || got[2].Level != Info || got[2].Kind != KindSystem || len(got[2].Text) != maxText+3 {
		t.Fatalf("got %+v", got)
	}
}
