package order

import "time"

// Ticket numbers (contract/agent.md §2.10ae, the owner 2026-11-13): the
// kitchen sequence (accepted online / website orders, from main's
// kitchen_start, default 1100) and the counter sequence (terminal and call-
// center orders, from counter_start, default 1) restart once a business day:
// Close batch marks them closed, and the first number taken on a new business
// day after a close starts both again. A second or third close the same day
// carries on, so a day never shows two #1s; a batch never restarts mid-way.

// DefaultKitchenStart is the kitchen sequence's start when main's terminal
// config doesn't say (the Pi's fixed start before the setting).
const DefaultKitchenStart = 1100

// DefaultDayStart is the business day's start when the store has no
// auto-close time (main's default auto_close_batch_at).
const DefaultDayStart = "04:00"

// Numbering is what taking a ticket number needs: the business day it is
// taken on and where the kitchen sequence starts.
type Numbering struct {
	Day          string // YYYY-MM-DD (BusinessDay)
	KitchenStart int64
}

// BusinessDay is the store's business day at t: the date, in loc, of the day
// that began at dayStart ("HH:MM" store time; "" or bad = DefaultDayStart) at
// or before t. With 04:00, 01:30 on the 14th is still the 13th's.
func BusinessDay(t time.Time, loc *time.Location, dayStart string) string {
	if loc == nil {
		loc = time.Local
	}
	hm, err := time.Parse("15:04", dayStart)
	if err != nil || len(dayStart) != 5 {
		hm, _ = time.Parse("15:04", DefaultDayStart)
	}
	local := t.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
	if local.Before(start) {
		local = local.AddDate(0, 0, -1)
	}
	return local.Format("2006-01-02")
}
