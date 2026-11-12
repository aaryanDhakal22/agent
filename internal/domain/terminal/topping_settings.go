package terminal

import "encoding/json"

// DefaultToppingMaxCount / MaxToppingMaxCount: main's topping_max_count
// default and ceiling (staff.Settings).
const (
	DefaultToppingMaxCount = 3
	MaxToppingMaxCount     = 10
)

// ToppingMaxCount is main's terminal setting topping_max_count: one option
// tapped again and again (2×, 3×) goes on at most this many times on the
// same portion + mode, in a group that allows repeats (the Pi's engine
// gets it as pricing.Context.MaxRepeat). Missing (or no config) =
// DefaultToppingMaxCount; bounded 1-10.
func (c *Config) ToppingMaxCount() int {
	if c == nil || len(c.Settings) == 0 {
		return DefaultToppingMaxCount
	}
	var s struct {
		V *int `json:"topping_max_count"`
	}
	if err := json.Unmarshal(c.Settings, &s); err != nil || s.V == nil {
		return DefaultToppingMaxCount
	}
	return min(max(*s.V, 1), MaxToppingMaxCount)
}
