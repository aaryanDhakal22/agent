package terminal

import (
	"encoding/json"
	"testing"
)

func TestLimitsOf(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"version":3,"settings":{"cashier_refund_cents":500,"cashier_discount_pct":15,"counter_start":40},
	 "staff":[{"id":"a","role":"cashier"},{"id":"b","role":"cashier","effective_limits":{"refund_cents":9,"discount_pct":1}},
	          {"id":"c","role":"driver","effective_limits":null},{"id":"d","role":"manager"},{"id":"e","role":"driver"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Limits{"a": {RefundCents: 500, DiscountPct: 15}, "b": {RefundCents: 9, DiscountPct: 1},
		"c": {Unlimited: true}, "d": {Unlimited: true}, "e": {}}
	for id, w := range want {
		s, _ := cfg.Find(id)
		if got := cfg.LimitsOf(s); got != w {
			t.Errorf("%s: %+v want %+v", id, got, w)
		}
	}
	if cfg.CounterStart() != 40 {
		t.Fatal(cfg.CounterStart())
	}
}

// main's terminal-app settings (default_tab, button_font, card tips, …) reach
// the terminals exactly as main sent them: the Pi reads only its own fields.
func TestConfigPassesAppSettingsThrough(t *testing.T) {
	body := `{"version": 3, "staff": [], "settings": {"auto_lock_minutes": 5, "default_tab": "takeout", "show_history": true,
		"button_font": "barlow_condensed", "cashier_price_drop_pct": 20, "click_sound": true, "prices_on_buttons": false,
		"out_of_area_margin_mi": 1.5, "name_on_paid_counter": false, "card_tips": true, "card_tip_percents": [15, 18, 20],
		"employee_meal_pct": 50, "a_future_field": {"x": 1}}}`
	c, err := ParseConfig([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(c.Settings, &got); err != nil {
		t.Fatal(err)
	}
	if got["default_tab"] != "takeout" || got["button_font"] != "barlow_condensed" || got["out_of_area_margin_mi"] != 1.5 ||
		got["card_tips"] != true || len(got["card_tip_percents"].([]any)) != 3 || got["a_future_field"] == nil {
		t.Fatalf("settings: %v", got)
	}
}
