package storeconfig

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Opening hours, ported from main's domain/tenancy/hours.go (keep the two in
// step). main validates the schedule when a manager saves it and ships it to
// the agent inside location.config (store_config frame), so the agent can
// tell "no orders while open" from "closed" without asking main.

// OpeningHoursKey is where a location's weekly schedule lives in its config.
const OpeningHoursKey = "opening_hours"

// Weekdays in schedule order (keys of OpeningHours).
var Weekdays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// HoursRange is one open period on a day. A close at or before open runs past
// midnight (e.g. 11:00-01:00).
type HoursRange struct {
	Open  string `json:"open"`
	Close string `json:"close"`
}

// OpeningHours maps "mon".."sun" to that day's open periods (none = closed).
type OpeningHours map[string][]HoursRange

func parseHHMM(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil || len(strings.TrimSpace(s)) != 5 {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Validate checks day keys and HH:MM times.
func (h OpeningHours) Validate() error {
	for day, ranges := range h {
		known := false
		for _, d := range Weekdays {
			if d == day {
				known = true
			}
		}
		if !known {
			return fmt.Errorf("opening_hours: unknown day %q (use mon..sun)", day)
		}
		for _, r := range ranges {
			o, err := parseHHMM(r.Open)
			if err != nil {
				return fmt.Errorf("opening_hours %s: %w", day, err)
			}
			c, err := parseHHMM(r.Close)
			if err != nil {
				return fmt.Errorf("opening_hours %s: %w", day, err)
			}
			if o == c {
				return fmt.Errorf("opening_hours %s: open and close are the same", day)
			}
		}
	}
	return nil
}

// HoursFromConfig reads (and validates) the schedule from a location config.
// ok=false when none is set.
func HoursFromConfig(cfg map[string]any) (OpeningHours, bool, error) {
	raw, present := cfg[OpeningHoursKey]
	if !present || raw == nil {
		return nil, false, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, false, err
	}
	var h OpeningHours
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, false, fmt.Errorf("opening_hours: %w", err)
	}
	if err := h.Validate(); err != nil {
		return nil, false, err
	}
	return h, len(h) > 0, nil
}

// IsOpen reports whether `at` (any zone) falls inside the schedule in loc,
// widened by `grace` on both ends. Periods past midnight count on the day they
// started.
func (h OpeningHours) IsOpen(at time.Time, loc *time.Location, grace time.Duration) bool {
	if loc == nil {
		loc = time.UTC
	}
	local := at.In(loc)
	for back := 0; back <= 1; back++ { // today's periods, then yesterday's overnight ones
		day := local.AddDate(0, 0, -back)
		key := Weekdays[(int(day.Weekday())+6)%7]
		for _, r := range h[key] {
			o, err1 := parseHHMM(r.Open)
			c, err2 := parseHHMM(r.Close)
			if err1 != nil || err2 != nil {
				continue
			}
			start := time.Date(day.Year(), day.Month(), day.Day(), o/60, o%60, 0, 0, loc)
			end := time.Date(day.Year(), day.Month(), day.Day(), c/60, c%60, 0, 0, loc)
			if c <= o {
				end = end.AddDate(0, 0, 1)
			}
			if !local.Before(start.Add(-grace)) && local.Before(end.Add(grace)) {
				return true
			}
		}
	}
	return false
}

// OpenNow reports whether the store is open at `at` in its own timezone.
// known=false when no (valid) schedule is configured — callers report that as
// "unknown", not "closed".
func (s *StoreContext) OpenNow(at time.Time) (open, known bool) {
	h, ok, err := HoursFromConfig(s.Location.Config)
	if err != nil || !ok {
		return false, false
	}
	loc := time.UTC
	if s.Location.Timezone != "" {
		if l, err := time.LoadLocation(s.Location.Timezone); err == nil {
			loc = l
		}
	}
	return h.IsOpen(at, loc, 0), true
}

// StoreName is the name shown to staff: the location's, else the tenant's.
func (s *StoreContext) StoreName() string {
	if s.Location.Name != "" {
		return s.Location.Name
	}
	return s.Tenant.Name
}
