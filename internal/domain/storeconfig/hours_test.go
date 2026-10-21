package storeconfig

import (
	"testing"
	"time"
)

func TestOpeningHours(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	h := OpeningHours{
		"mon": {{Open: "11:00", Close: "23:00"}},
		"fri": {{Open: "11:00", Close: "01:00"}}, // past midnight
		"sun": {{Open: "17:00", Close: "02:30"}}, // overnight into a new week
	}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, ny)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// 2026-10-19 is a Monday, 2026-10-23 a Friday, 2026-10-25 a Sunday.
	for _, c := range []struct {
		when  string
		grace time.Duration
		open  bool
	}{
		{"2026-10-19 10:00", 15 * time.Minute, false},
		{"2026-10-19 10:50", 15 * time.Minute, true}, // grace before open
		{"2026-10-19 10:50", 0, false},
		{"2026-10-19 12:00", 0, true},
		{"2026-10-19 23:10", 15 * time.Minute, true}, // grace after close
		{"2026-10-19 23:10", 0, false},
		{"2026-10-19 23:20", 15 * time.Minute, false},
		{"2026-10-20 12:00", 0, false}, // tuesday closed
		{"2026-10-24 00:30", 0, true},  // friday's overnight period on saturday
		{"2026-10-24 01:20", 15 * time.Minute, false},
		{"2026-10-26 02:00", 0, true}, // sunday's overnight period on monday
		{"2026-10-26 02:45", 0, false},
	} {
		if got := h.IsOpen(at(c.when), ny, c.grace); got != c.open {
			t.Errorf("%s grace %v: open=%v, want %v", c.when, c.grace, got, c.open)
		}
	}
	// The instant's own zone doesn't matter: 16:00 UTC is noon in New York.
	if !h.IsOpen(time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC), ny, 0) {
		t.Error("UTC instant should be judged in the store's zone")
	}
	bad := []OpeningHours{
		{"funday": {{Open: "11:00", Close: "12:00"}}},
		{"mon": {{Open: "25:00", Close: "12:00"}}},
		{"mon": {{Open: "11:00", Close: "11:00"}}},
		{"mon": {{Open: "9:00", Close: "12:00"}}},
	}
	for _, b := range bad {
		if b.Validate() == nil {
			t.Errorf("%v should not validate", b)
		}
	}
	if _, ok, err := HoursFromConfig(map[string]any{}); ok || err != nil {
		t.Errorf("empty config: ok=%v err=%v", ok, err)
	}
	cfg := map[string]any{OpeningHoursKey: map[string]any{"mon": []any{map[string]any{"open": "11:00", "close": "22:00"}}}}
	if got, ok, err := HoursFromConfig(cfg); !ok || err != nil || len(got["mon"]) != 1 {
		t.Errorf("config hours: %v %v %v", got, ok, err)
	}
}

func TestStoreContextOpenNow(t *testing.T) {
	sc := StoreContext{Location: LocationContext{Name: "Lombardi's", Timezone: "America/New_York",
		Config: map[string]any{OpeningHoursKey: map[string]any{
			"mon": []any{map[string]any{"open": "11:00", "close": "22:00"}},
		}}}}
	if open, known := sc.OpenNow(time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC)); !open || !known {
		t.Errorf("monday noon NY: open=%v known=%v", open, known)
	}
	if open, known := sc.OpenNow(time.Date(2026, 10, 20, 16, 0, 0, 0, time.UTC)); open || !known {
		t.Errorf("tuesday: open=%v known=%v", open, known)
	}
	none := StoreContext{}
	if _, known := none.OpenNow(time.Now()); known {
		t.Error("no schedule must be unknown")
	}
	bad := StoreContext{Location: LocationContext{Config: map[string]any{OpeningHoursKey: "always"}}}
	if _, known := bad.OpenNow(time.Now()); known {
		t.Error("a malformed schedule must be unknown")
	}
	if sc.StoreName() != "Lombardi's" || (&StoreContext{Tenant: TenantContext{Name: "T"}}).StoreName() != "T" {
		t.Error("store name fallback")
	}
}
