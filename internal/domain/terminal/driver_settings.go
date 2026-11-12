package terminal

import (
	"encoding/json"

	"quiccpos/agent/internal/domain/order"
)

// MaxDriverClaimAfterMinutes bounds main's driver_claim_after_minutes.
const MaxDriverClaimAfterMinutes = 480

// DriverClaimAfterMinutes is main's terminal setting
// driver_claim_after_minutes: how long a delivery waits unassigned before a
// driver may claim it in his Dispatch section (contract/agent.md §2.10b).
// Missing (or no config) = order.DefaultClaimAfterMinutes (45); bounded
// 0-480.
func (c *Config) DriverClaimAfterMinutes() int {
	if c == nil || len(c.Settings) == 0 {
		return order.DefaultClaimAfterMinutes
	}
	var s struct {
		V *int `json:"driver_claim_after_minutes"`
	}
	if err := json.Unmarshal(c.Settings, &s); err != nil || s.V == nil {
		return order.DefaultClaimAfterMinutes
	}
	return min(max(*s.V, 0), MaxDriverClaimAfterMinutes)
}
