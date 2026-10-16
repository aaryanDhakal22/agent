package transport

import (
	"strings"
	"testing"
	"time"
)

func TestPairingCodes(t *testing.T) {
	var p pairingCodes
	now := time.Now()
	code, exp, err := p.issue(now)
	if err != nil || len(code) != 6 || exp.Sub(now) != pairingTTL {
		t.Fatalf("issue: %q %v %v", code, exp, err)
	}
	if !p.consume(code, now) {
		t.Fatal("right code should pair")
	}
	if p.consume(code, now) {
		t.Fatal("a code is single-use")
	}

	code, _, _ = p.issue(now)
	if p.consume(code, now.Add(pairingTTL+time.Second)) {
		t.Fatal("expired code must fail")
	}

	code, _, _ = p.issue(now)
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for i := 0; i < pairingMaxMisses; i++ {
		p.consume(wrong, now)
	}
	if p.consume(code, now) {
		t.Fatal("too many wrong guesses must burn the code")
	}
}

func TestDeviceKeys(t *testing.T) {
	a, _ := newDeviceKey()
	b, _ := newDeviceKey()
	if a == b || !strings.HasPrefix(a, deviceKeyPrefix) || len(a) < 40 {
		t.Fatalf("keys: %q %q", a, b)
	}
	if hashDeviceKey(a) == hashDeviceKey(b) || len(hashDeviceKey(a)) != 64 {
		t.Fatal("hash")
	}
}
