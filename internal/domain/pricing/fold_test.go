package pricing

import "testing"

// Mods never stack (the owner, 2026-11-14, #20): every option is its own
// child; an attached qualifier is left out and its price goes to its pick.
func TestModLines(t *testing.T) {
	in := []ResolvedOption{
		{Name: "Pepperoni", Label: "Pepperoni", Mode: ModeAdd, PriceCents: 150, Count: 2},
		{Name: "Pepperoni", Label: "Pepperoni", Mode: ModeAdd, PriceCents: 150},
		{Name: "Lite", Label: "Lite", Mode: ModeAdd, PriceCents: 10, Qualifier: true, Attached: true},
		{Name: "Fried Onions", Label: "Lite Fried Onions", Mode: ModeAdd, PriceCents: 0},
		{Name: "Lite", Label: "Lite", Mode: ModeAdd, Qualifier: true, Attached: true},
		{Name: "Mayo", Label: "Lite Mayo", Mode: ModeAdd},
		{Name: "Onion", Label: "NO Onion", Mode: ModeNo, RemovedDefault: true},
		{Name: "Extra", Label: "Extra", Mode: ModeAdd, Qualifier: true},
	}
	got := ModLines(in)
	want := []struct {
		label string
		price int64
	}{{"Pepperoni", 150}, {"Pepperoni", 150}, {"Lite Fried Onions", 10}, {"Lite Mayo", 0}, {"NO Onion", 0}, {"Extra", 0}}
	if len(got) != len(want) {
		t.Fatalf("got %d: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].Label != w.label || got[i].PriceCents != w.price || got[i].Count != 1 {
			t.Errorf("%d: got %q %d ×%d, want %q %d", i, got[i].Label, got[i].PriceCents, got[i].Count, w.label, w.price)
		}
	}
}

func TestIsQualifier(t *testing.T) {
	for _, n := range []string{"Lite", " lite ", "EXTRA", "Xtra", "Light", "Easy", "Less", "Heavy"} {
		if !IsQualifier(n) {
			t.Errorf("%q", n)
		}
	}
	for _, n := range []string{"Extra Cheese", "Lite Mayo", "Mayo", ""} {
		if IsQualifier(n) {
			t.Errorf("%q", n)
		}
	}
}
