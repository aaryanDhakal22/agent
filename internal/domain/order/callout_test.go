package order

import "testing"

// 2.3.4: repeat is 1-3; settings stored before it (no repeat) say it once.
func TestCalloutRepeat(t *testing.T) {
	if DefaultCallout().Repeat != 1 {
		t.Fatalf("default repeat = %d, want 1", DefaultCallout().Repeat)
	}
	for _, n := range []int{1, 2, 3} {
		c := DefaultCallout()
		c.Repeat = n
		if err := c.Validate(); err != nil {
			t.Fatalf("repeat %d: %v", n, err)
		}
	}
	for _, n := range []int{-1, 0, 4} {
		c := DefaultCallout()
		c.Repeat = n
		if err := c.Validate(); err == nil {
			t.Fatalf("repeat %d accepted", n)
		}
	}
	old, err := DecodeCallout([]byte(`{"enabled":true,"voice":"kokoro:bf_emma","volume":80,"music_dip":40,"restore_seconds":2,"chime":false}`))
	if err != nil || old.Repeat != 1 || old.Voice != "kokoro:bf_emma" || old.Validate() != nil {
		t.Fatalf("pre-2.3.4 settings = %+v %v", old, err)
	}
	got, err := DecodeCallout([]byte(`{"repeat":2}`))
	if err != nil || got.Repeat != 2 {
		t.Fatalf("stored repeat = %+v %v", got, err)
	}
	if err := ValidateVoice("kokoro:af_heart"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateVoice("../x"); err == nil {
		t.Fatal("bad voice accepted")
	}
}
