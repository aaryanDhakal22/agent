package transport

import (
	"encoding/json"
	"testing"

	"quiccpos/agent/internal/domain/order"
)

// A PUT keeps what it leaves out, but voice_lines replaces the whole map:
// a voice whose line was cleared is left out of it (2.3.8).
func TestCalloutPutReplacesVoiceLines(t *testing.T) {
	cur := order.DefaultCallout()
	cur.Volume, cur.VoiceLines = 80, map[string]string{"kokoro:am_onyx": "Hot!", "kokoro:bm_george": "Cheerio <x>"}
	keep := cur
	if err := json.Unmarshal([]byte(`{"repeat":2}`), &calloutPut{&keep}); err != nil {
		t.Fatal(err)
	}
	if keep.Repeat != 2 || keep.Volume != 80 || len(keep.VoiceLines) != 2 {
		t.Fatalf("missing keys keep their value: %+v", keep)
	}
	repl := cur
	if err := json.Unmarshal([]byte(`{"voice_lines":{"kokoro:bm_george":"Cheerio <x>"}}`), &calloutPut{&repl}); err != nil {
		t.Fatal(err)
	}
	if len(repl.VoiceLines) != 1 || repl.VoiceLines["kokoro:bm_george"] != "Cheerio <x>" {
		t.Fatalf("voice_lines replaced: %+v", repl.VoiceLines)
	}
	if len(cur.VoiceLines) != 2 {
		t.Fatal("the current settings' map was changed in place")
	}
	none := cur
	if err := json.Unmarshal([]byte(`{"voice_lines":null}`), &calloutPut{&none}); err != nil || none.VoiceLines == nil || len(none.VoiceLines) != 0 {
		t.Fatalf("null clears: %+v %v", none.VoiceLines, err)
	}
}
