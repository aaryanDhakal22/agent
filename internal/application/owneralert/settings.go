// Package owneralert decides what reaches the store owner's phone (2.5.2).
//
// The owner (the boss key + the owner's devices, notify.SendOwner) gets only
// a handful of plain-English, actionable alerts — never the developer's
// stream:
//
//   - a printer down for printer_down minutes (once per printer per outage,
//     plus one "working again" when it was told about the outage);
//   - an order that printed on no printer (once per order);
//   - the Pi without main for offline minutes (once per outage + recovery);
//   - orders waiting to be accepted (once per incident);
//   - yesterday's batch still open in the morning (once a day).
//
// Every alert goes through Policy: only inside opening hours (store_config's
// opening_hours; unknown = 09:00-02:00), a dedup key with a cooldown (>= 30
// min), at most max_per_hour / max_per_day messages, and one summary of what
// the caps held back. The developer's own alerts (notify.Send / SendFloor)
// are untouched. Contract §2.1e "Owner alerts", spec §4.53.
package owneralert

import (
	"time"

	"quiccpos/agent/internal/domain/storeconfig"
)

// Location config keys (main's Store pages / the admin location PATCH; read
// from store_config, so a change needs no release). Each is clamped.
const (
	KeyEnabled            = "owner_alerts"                     // bool, default true
	KeyPrinterDownMinutes = "owner_alert_printer_down_minutes" // 5, 1-120
	KeyOfflineMinutes     = "owner_alert_offline_minutes"      // 5, 1-120
	KeyWaitingOrders      = "owner_alert_waiting_orders"       // 2, 1-50
	KeyWaitingMinutes     = "owner_alert_waiting_minutes"      // 5, 1-120
	KeyOrderWaitMinutes   = "owner_alert_order_wait_minutes"   // 15, 1-240
	KeyCloseCheckHour     = "owner_alert_close_check_hour"     // 10, 0-23
	KeyCooldownMinutes    = "owner_alert_cooldown_minutes"     // 30, 30-1440
	KeyMaxPerHour         = "owner_alert_max_per_hour"         // 6, 1-60
	KeyMaxPerDay          = "owner_alert_max_per_day"          // 20, 1-200
)

// Settings are the owner-alert thresholds.
type Settings struct {
	Enabled bool
	// PrinterDown: a printer failing without a break this long is down.
	PrinterDown time.Duration
	// Offline: the Pi without main this long is an outage.
	Offline time.Duration
	// WaitingOrders orders each waiting over WaitingFor = a pile-up; one
	// order waiting over OrderWait is one too.
	WaitingOrders int
	WaitingFor    time.Duration
	OrderWait     time.Duration
	// CloseCheckHour: the store-local hour the morning batch check runs.
	CloseCheckHour int
	// Cooldown: the same alert (dedup key) again only after this long.
	Cooldown   time.Duration
	MaxPerHour int
	MaxPerDay  int
}

// Defaults (the owner, 2026-11-12).
func Defaults() Settings {
	return Settings{
		Enabled:        true,
		PrinterDown:    5 * time.Minute,
		Offline:        5 * time.Minute,
		WaitingOrders:  2,
		WaitingFor:     5 * time.Minute,
		OrderWait:      15 * time.Minute,
		CloseCheckHour: 10,
		Cooldown:       30 * time.Minute,
		MaxPerHour:     6,
		MaxPerDay:      20,
	}
}

// MinCooldown: the cooldown is never shorter than this.
const MinCooldown = 30 * time.Minute

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// SettingsFrom reads the thresholds from a store's location config (the
// defaults for a key that is missing).
func SettingsFrom(sc *storeconfig.StoreContext) Settings {
	s := Defaults()
	if sc == nil {
		return s
	}
	if v, ok := sc.Location.Config[KeyEnabled].(bool); ok {
		s.Enabled = v
	}
	mins := func(key string, def time.Duration, lo, hi int) time.Duration {
		return time.Duration(clamp(sc.LocationConfigInt(key, int(def/time.Minute)), lo, hi)) * time.Minute
	}
	s.PrinterDown = mins(KeyPrinterDownMinutes, s.PrinterDown, 1, 120)
	s.Offline = mins(KeyOfflineMinutes, s.Offline, 1, 120)
	s.WaitingOrders = clamp(sc.LocationConfigInt(KeyWaitingOrders, s.WaitingOrders), 1, 50)
	s.WaitingFor = mins(KeyWaitingMinutes, s.WaitingFor, 1, 120)
	s.OrderWait = mins(KeyOrderWaitMinutes, s.OrderWait, 1, 240)
	s.CloseCheckHour = clamp(sc.LocationConfigInt(KeyCloseCheckHour, s.CloseCheckHour), 0, 23)
	s.Cooldown = mins(KeyCooldownMinutes, s.Cooldown, int(MinCooldown/time.Minute), 1440)
	s.MaxPerHour = clamp(sc.LocationConfigInt(KeyMaxPerHour, s.MaxPerHour), 1, 60)
	s.MaxPerDay = clamp(sc.LocationConfigInt(KeyMaxPerDay, s.MaxPerDay), 1, 200)
	return s
}

// Fallback opening hours when the store has none (or invalid ones):
// 09:00 to 02:00, every day.
var fallbackHours = storeconfig.OpeningHours{
	"mon": {{Open: "09:00", Close: "02:00"}}, "tue": {{Open: "09:00", Close: "02:00"}},
	"wed": {{Open: "09:00", Close: "02:00"}}, "thu": {{Open: "09:00", Close: "02:00"}},
	"fri": {{Open: "09:00", Close: "02:00"}}, "sat": {{Open: "09:00", Close: "02:00"}},
	"sun": {{Open: "09:00", Close: "02:00"}},
}

// Location is the store's time zone (the Pi's own when unknown).
func Location(sc *storeconfig.StoreContext) *time.Location {
	if sc != nil && sc.Location.Timezone != "" {
		if l, err := time.LoadLocation(sc.Location.Timezone); err == nil {
			return l
		}
	}
	return time.Local
}

// OpenAt reports whether owner alerts may go out at `at`: inside the store's
// opening hours, else inside the 09:00-02:00 fallback.
func OpenAt(sc *storeconfig.StoreContext, at time.Time) bool {
	loc := Location(sc)
	if sc != nil {
		if h, ok, err := storeconfig.HoursFromConfig(sc.Location.Config); err == nil && ok {
			return h.IsOpen(at, loc, 0)
		}
	}
	return fallbackHours.IsOpen(at, loc, 0)
}
