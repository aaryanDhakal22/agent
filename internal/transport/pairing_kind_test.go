package transport

import (
	"errors"
	"testing"
	"time"
)

// X-1 (contract 2.10s §8): a code is bound to its minter's kind, a code for
// another kind is burned, and a device mints at most 5 codes per 15 min.
func TestPairingCodeKindAndLimit(t *testing.T) {
	var p pairingCodes
	now := time.Now()
	code, _, err := p.issueFor("tab-1", "tablet", now)
	if err != nil {
		t.Fatal(err)
	}
	if ok, wrong := p.consumeKind(code, "terminal", now); ok || !wrong {
		t.Fatalf("a tablet's code for a terminal: ok=%v wrong=%v", ok, wrong)
	}
	if ok, _ := p.consumeKind(code, "tablet", now); ok {
		t.Fatal("the code was burned")
	}
	code, _, _ = p.issueFor("tab-1", "tablet", now)
	if ok, wrong := p.consumeKind(code, "tablet", now); !ok || wrong {
		t.Fatal("a tablet code pairs a tablet")
	}
	// The session's code without a kind pairs anything.
	code, _, _ = p.issueFor("web", "", now)
	if ok, _ := p.consumeKind(code, "terminal", now); !ok {
		t.Fatal("a web code pairs a terminal")
	}
	// Rate limit: tab-1 minted 2; 3 more, then 429 until the window passes.
	for i := 0; i < 3; i++ {
		if _, _, err := p.issueFor("tab-1", "tablet", now); err != nil {
			t.Fatalf("code %d: %v", i, err)
		}
	}
	var many *errTooManyCodes
	if _, _, err := p.issueFor("tab-1", "tablet", now); !errors.As(err, &many) || many.wait <= 0 {
		t.Fatalf("sixth code: %v", err)
	}
	if _, _, err := p.issueFor("tab-2", "tablet", now); err != nil {
		t.Fatalf("another tablet has its own count: %v", err)
	}
	if _, _, err := p.issueFor("tab-1", "tablet", now.Add(pairingCodesWindow+time.Second)); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}
