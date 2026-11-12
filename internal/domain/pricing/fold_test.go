package pricing

import "testing"

func TestFoldRepeats(t *testing.T) {
	in := []ResolvedOption{
		{Name: "Pepperoni", Label: "Pepperoni", Mode: ModeAdd, PriceCents: 150},
		{Name: "Onion", Label: "NO Onion", Mode: ModeNo, RemovedDefault: true},
		{Name: "Pepperoni", Label: "Pepperoni (1st half)", Portion: PortionFirst, Mode: ModeAdd, PriceCents: 75},
		{Name: "Pepperoni", Label: "Pepperoni", Mode: ModeAdd, PriceCents: 150},
		{Name: "Onion", Label: "NO Onion", Mode: ModeNo, RemovedDefault: true},
		{Name: "Pepperoni", Label: "Pepperoni", Mode: ModeAdd, PriceCents: 150},
		{Name: "Pepperoni", Label: "Pepperoni (1st half)", Portion: PortionFirst, Mode: ModeAdd, PriceCents: 75},
		{Name: "Ham", Label: "Ham", Mode: ModeAdd, PriceCents: 150, Included: true},
		{Name: "Ham", Label: "Ham", Mode: ModeAdd, PriceCents: 150},
	}
	got := FoldRepeats(in)
	want := []struct {
		label string
		count int
	}{{"3x Pepperoni", 3}, {"NO Onion", 1}, {"2x Pepperoni (1st half)", 2}, {"NO Onion", 1}, {"Ham", 1}, {"Ham", 1}}
	if len(got) != len(want) {
		t.Fatalf("got %d options: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].Label != w.label || got[i].Count != w.count {
			t.Errorf("%d: got %q ×%d, want %q ×%d", i, got[i].Label, got[i].Count, w.label, w.count)
		}
	}
	if got[0].PriceCents != 150 {
		t.Errorf("the price stays one's: %d", got[0].PriceCents)
	}
}
