package settingslog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDiffFlatSection(t *testing.T) {
	type tickets struct {
		PosDelivery bool `json:"pos_delivery"`
		OnlineCard  bool `json:"online_card"`
		OnlineCash  bool `json:"online_cash_delivery"`
	}
	got := Diff(tickets{OnlineCard: true, OnlineCash: true}, tickets{OnlineCard: false, OnlineCash: true})
	if len(got) != 1 || got[0].Key != "online_card" || got[0].From != true || got[0].To != false {
		t.Fatalf("diff: %+v", got)
	}
	if got := Diff(tickets{OnlineCard: true}, tickets{OnlineCard: true}); len(got) != 0 {
		t.Fatalf("no change: %+v", got)
	}
}

func TestDiffNestedMapsNumbersAndNil(t *testing.T) {
	before := map[string]any{"volume": 90, "voice_lines": map[string]string{"kokoro:af_heart": "Order <x>"}, "voice": nil}
	after := map[string]any{"volume": 80, "voice_lines": map[string]string{"kokoro:am_onyx": "Number <x>"}, "voice": "kokoro:am_onyx"}
	got := Diff(before, after)
	want := []string{"voice", "voice_lines.kokoro:af_heart", "voice_lines.kokoro:am_onyx", "volume"}
	if len(got) != len(want) {
		t.Fatalf("diff: %+v", got)
	}
	for i, k := range want {
		if got[i].Key != k {
			t.Fatalf("key %d = %q, want %q (%+v)", i, got[i].Key, k, got)
		}
	}
	if got[0].From != nil || got[0].To != "kokoro:am_onyx" {
		t.Fatalf("voice: %+v", got[0])
	}
	if got[1].To != nil || got[2].From != nil {
		t.Fatalf("one-sided keys: %+v", got)
	}
	b, _ := json.Marshal(got[3])
	if string(b) != `{"key":"volume","from":90,"to":80}` {
		t.Fatalf("number: %s", b)
	}
}

func TestDiffCutsLongStrings(t *testing.T) {
	long := strings.Repeat("x", 300)
	got := Diff(map[string]string{"footer": ""}, map[string]string{"footer": long})
	if s, _ := got[0].To.(string); len([]rune(s)) != maxValue || !strings.HasSuffix(s, "…") {
		t.Fatalf("long value: %d %q", len([]rune(s)), s)
	}
}

func TestEntryText(t *testing.T) {
	e := Entry{Section: "tickets", By: By{Kind: ByTablet, Name: "Counter"}, Changes: []Change{
		{Key: "online_card", From: true, To: false}, {Key: "online_cash_delivery", From: false, To: true}}}
	if got := e.Text(); got != "Kitchen tickets: online_card on → off, online_cash_delivery off → on (tablet Counter)" {
		t.Fatalf("text: %q", got)
	}
	e = Entry{Section: "auto-accept", By: By{Kind: ByWeb, Name: "Pi web page"}, Changes: []Change{{Key: "auto_accept", From: false, To: true}}}
	if got := e.Text(); got != "Auto-accept: off → on (Pi web page)" {
		t.Fatalf("text: %q", got)
	}
}
