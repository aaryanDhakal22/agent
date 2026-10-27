package order

import "testing"

// Store, 2026-10-26 (#314): a check stored before its lines carried pos_ref,
// pulled again with them, is the same check — not every line removed + added.
func TestPOSDiffMixedRefsMatchesByName(t *testing.T) {
	before := []LineItem{{Name: "Grill Chicken Caesar", Quantity: 2}, {Name: "Jalapeno Poppers", Quantity: 1}, {Name: "Blue Cheese", Quantity: 1}}
	after := []LineItem{{Name: "Grill Chicken Caesar", Quantity: 2, PosRef: "11"}, {Name: "Jalapeno Poppers", Quantity: 1, PosRef: "12"}, {Name: "Blue Cheese", Quantity: 1, PosRef: "13"}}
	added, removed, _, _ := POSDiff(before, after)
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("added %v removed %v, want none", added, removed)
	}
	after = append(after, LineItem{Name: "Garlic Knots", Quantity: 1, PosRef: "14", Kitchen: true})
	added, removed, _, _ = POSDiff(before, after)
	if len(added) != 1 || added[0].Name != "Garlic Knots" || len(removed) != 0 {
		t.Fatalf("added %v removed %v, want only Garlic Knots", added, removed)
	}
}
