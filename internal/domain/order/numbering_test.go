package order

import (
	"testing"
	"time"
)

func TestBusinessDay(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, ny)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		at, start, want string
	}{
		{"2026-11-13 11:00", "04:00", "2026-11-13"},
		{"2026-11-13 23:59", "04:00", "2026-11-13"},
		{"2026-11-14 01:30", "04:00", "2026-11-13"}, // after midnight, before the day starts
		{"2026-11-14 03:59", "04:00", "2026-11-13"},
		{"2026-11-14 04:00", "04:00", "2026-11-14"},
		{"2026-11-14 01:30", "", "2026-11-13"},      // no auto-close: 04:00
		{"2026-11-14 01:30", "bad", "2026-11-13"},   // a bad value: 04:00
		{"2026-11-13 23:45", "23:30", "2026-11-13"}, // a day that starts late
		{"2026-11-13 23:15", "23:30", "2026-11-12"},
		{"2026-11-14 00:10", "00:00", "2026-11-14"},
	}
	for _, c := range cases {
		if got := BusinessDay(at(c.at), ny, c.start); got != c.want {
			t.Errorf("BusinessDay(%s, %q) = %s, want %s", c.at, c.start, got, c.want)
		}
	}
	// The store's timezone decides, not the Pi's clock zone.
	utc := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC) // 00:00 in New York
	if got := BusinessDay(utc, ny, "04:00"); got != "2026-11-13" {
		t.Errorf("UTC instant: %s", got)
	}
}
