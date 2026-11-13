package safego

import (
	"testing"
	"time"
)

func TestStartPageDue(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	if !StartPageDue(dir, t0, 15*time.Minute) {
		t.Fatal("first start must page")
	}
	if StartPageDue(dir, t0.Add(5*time.Second), 15*time.Minute) || StartPageDue(dir, t0.Add(14*time.Minute), 15*time.Minute) {
		t.Fatal("a restart within 15 min must not page")
	}
	if !StartPageDue(dir, t0.Add(15*time.Minute), 15*time.Minute) {
		t.Fatal("after 15 min it pages again")
	}
}
