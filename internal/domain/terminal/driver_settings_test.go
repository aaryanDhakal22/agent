package terminal

import (
	"testing"
	"time"
)

// #27: main's driver_old_delivery_minutes (default 60, 0 = off, capped at 720).
func TestDriverOldDeliverySetting(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]time.Duration{
		`{"version":1,"staff":[],"settings":{}}`:                                   60 * time.Minute,
		`{"version":1,"staff":[],"settings":{"driver_old_delivery_minutes":90}}`:   90 * time.Minute,
		`{"version":1,"staff":[],"settings":{"driver_old_delivery_minutes":0}}`:    0,
		`{"version":1,"staff":[],"settings":{"driver_old_delivery_minutes":9999}}`: 720 * time.Minute,
	} {
		c, err := ParseConfig([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.DriverOldDelivery(); got != want {
			t.Fatalf("%s: %v, want %v", body, got, want)
		}
	}
	var none *Config
	if none.DriverOldDelivery() != 60*time.Minute {
		t.Fatal("no config = the default")
	}
}
