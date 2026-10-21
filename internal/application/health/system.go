package health

import (
	"strconv"
	"strings"
	"time"
)

// StaleAfter: the root timer rewrites HealthFile every minute, so a file
// older than this means the timer (or the clock) is broken.
const StaleAfter = 5 * time.Minute

// Bits of the firmware's get_throttled word (vcgencmd get_throttled).
const (
	bitUnderVoltageNow  = 1 << 0
	bitThrottledNow     = 1 << 2
	bitUnderVoltageBoot = 1 << 16
	bitThrottledBoot    = 1 << 18
)

// SystemHealth is HealthFile parsed: what the tablet's troubleshooting guides
// branch on. Fields the file didn't carry are omitted; without a file at all
// (dev, tests, a dead timer) FileStale is true and the rest is zero.
type SystemHealth struct {
	SDErrors              *int64 `json:"sd_errors,omitempty"`
	RootfsRO              *bool  `json:"rootfs_ro,omitempty"`
	Throttled             string `json:"throttled,omitempty"` // raw, e.g. "0x50005"
	UnderVoltageNow       bool   `json:"under_voltage_now"`
	UnderVoltageSinceBoot bool   `json:"under_voltage_since_boot"`
	ThrottledNow          bool   `json:"throttled_now"`
	ThrottledSinceBoot    bool   `json:"throttled_since_boot"`
	NTPSynced             string `json:"ntp_synced,omitempty"` // "yes" | "no" | "unknown"
	FileAt                string `json:"file_at,omitempty"`
	FileStale             bool   `json:"file_stale"`
	// FilePresent: a HealthFile was read (absent on a dev box).
	FilePresent bool `json:"-"`
}

// ParseThrottled reads a get_throttled value ("0x50005", "50005" or decimal
// from sysfs). ok=false for "", "unknown" or garbage.
func ParseThrottled(s string) (uint64, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "unknown" {
		return 0, false
	}
	if v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64); err == nil {
		return v, true
	}
	return 0, false
}

// ParseSystem turns the HealthFile map (nil when there is none) into a
// SystemHealth judged at now.
func ParseSystem(sys map[string]any, now time.Time) SystemHealth {
	out := SystemHealth{FileStale: true}
	if sys == nil {
		return out
	}
	out.FilePresent = true
	if n, ok := sys["sd_errors"].(float64); ok {
		v := int64(n)
		out.SDErrors = &v
	}
	if b, ok := sys["rootfs_ro"].(bool); ok {
		out.RootfsRO = &b
	}
	if th, ok := sys["throttled"].(string); ok {
		out.Throttled = th
		if bits, ok := ParseThrottled(th); ok {
			out.UnderVoltageNow = bits&bitUnderVoltageNow != 0
			out.UnderVoltageSinceBoot = bits&bitUnderVoltageBoot != 0
			out.ThrottledNow = bits&bitThrottledNow != 0
			out.ThrottledSinceBoot = bits&bitThrottledBoot != 0
		}
	}
	if v, ok := sys["ntp_synced"].(string); ok {
		out.NTPSynced = v
	}
	if at, ok := sys["at"].(string); ok {
		out.FileAt = at
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			out.FileStale = now.Sub(t) > StaleAfter
		}
	}
	return out
}
