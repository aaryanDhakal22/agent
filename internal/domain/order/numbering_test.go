package order

import (
	"math/rand/v2"
	"testing"
)

// Ticket numbers, the owner's final rule (2026-11-14, contract §2.10ae).

func TestNumberingStartDefaultsTo100(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want int64 }{{0, 100}, {-3, 100}, {1, 1}, {100, 100}, {250, 250}, {9_999, 9_999}, {10_000, 100}} {
		if got := (Numbering{Start: c.in}).StartOrDefault(); got != c.want {
			t.Errorf("start %d → %d, want %d", c.in, got, c.want)
		}
	}
}

func TestPickPlaceholderIsFiveDigitsAndFree(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	held := map[int64]bool{}
	for range 500 {
		n, ok, err := PickPlaceholder(rng.Int64N, func(c int64) (bool, error) { return held[c], nil })
		if err != nil || !ok {
			t.Fatalf("pick: %v %v", ok, err)
		}
		if n < 10_000 || n > 99_999 || !IsPlaceholder(n) {
			t.Fatalf("placeholder %d is not 5 digits", n)
		}
		if held[n] {
			t.Fatalf("placeholder %d handed out twice", n)
		}
		held[n] = true
	}
}

// A random source that keeps landing on held numbers still finds a free one
// (the walk after the draws), and a full range says so.
func TestPickPlaceholderNeverRepeatsAHeldOne(t *testing.T) {
	t.Parallel()
	same := func(int64) int64 { return 0 } // always 10000
	held := map[int64]bool{10_000: true, 10_001: true, 10_002: true}
	n, ok, err := PickPlaceholder(same, func(c int64) (bool, error) { return held[c], nil })
	if err != nil || !ok || n != 10_003 {
		t.Fatalf("got %d %v %v, want 10003", n, ok, err)
	}
	if _, ok, _ := PickPlaceholder(same, func(int64) (bool, error) { return true, nil }); ok {
		t.Fatal("every placeholder taken: want ok false")
	}
}
