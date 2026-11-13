package receipt

import "testing"

// The slips' item names (the owner, 2.5.2): Title Case keeping the menu's capitals, an inch size marked
// with ", counts and volumes left alone.
func TestSlipItemName(t *testing.T) {
	for _, c := range []struct{ size, name, want string }{
		{"", "8 Sub", `8" Sub`},
		{"12 Medium", "Cheese Pizza", `12" Medium Cheese Pizza`},
		{"16", "cheese pizza", `16" Cheese Pizza`},
		{"", "10 calzone", `10" Calzone`},
		{"8", "Italian Cold Cut", `8" Italian Cold Cut`}, // an inch size alone says inches
		{`12"`, "Pizza", `12" Pizza`},                    // already marked
		{"", "6 Wings", "6 Wings"},
		{"", "10 Nuggets", "10 Nuggets"},
		{"2 Liter", "Coca-Cola", "2 Liter Coca-Cola"},
		{"20 oz", "Sprite", "20 oz Sprite"},
		{"10 PC", "buffalo wings", "10 PC Buffalo Wings"},
		{"", "BBQ chicken BLT sub", "BBQ Chicken BLT Sub"},
		{"Large", "pepperoni pizza", "Large Pepperoni Pizza"},
		{"Serving", "garlic knots", "Garlic Knots"},
	} {
		if got := slipItemName(c.size, c.name); got != c.want {
			t.Errorf("%q + %q = %q, want %q", c.size, c.name, got, c.want)
		}
	}
}
