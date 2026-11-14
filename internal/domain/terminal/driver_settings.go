package terminal

import (
	"encoding/json"
	"time"
)

// DefaultDriverOldDeliveryMinutes is main's own default for
// driver_old_delivery_minutes (#27, batch F, contract 2.10am: the owner's X,
// 60 min).
const DefaultDriverOldDeliveryMinutes = 60

// DriverOldDelivery is how long a delivery stays undelivered before every
// driver may claim it (main's driver_old_delivery_minutes, 0-720; 0 = off;
// missing, an older main or no config yet = the default). Read from the raw
// settings so the parsed Settings stay untouched.
func (c *Config) DriverOldDelivery() time.Duration {
	mins := int64(DefaultDriverOldDeliveryMinutes)
	if c != nil && len(c.Settings) > 0 {
		var s struct {
			Minutes *int64 `json:"driver_old_delivery_minutes"`
		}
		if json.Unmarshal(c.Settings, &s) == nil && s.Minutes != nil {
			mins = min(max(*s.Minutes, 0), 720)
		}
	}
	return time.Duration(mins) * time.Minute
}
