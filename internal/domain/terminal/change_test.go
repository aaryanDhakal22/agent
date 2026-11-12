package terminal

import (
	"reflect"
	"testing"

	"quiccpos/agent/internal/domain/order"
)

func line(id, name string, qty int, mods ...string) order.LineItem {
	li := order.LineItem{ID: id, Name: name, Quantity: qty, UnitPrice: 1000}
	for i, m := range mods {
		li.Children = append(li.Children, order.LineItem{ID: id + "-c" + string(rune('0'+i)), Name: m, Quantity: qty, UnitPrice: 100})
	}
	return li
}

func TestDiffLines(t *testing.T) {
	before := []order.LineItem{line("a", "Cheese Pizza", 1, "Pepperoni"), line("b", "Soda", 2), line("c", "Wings", 1)}
	after := []order.LineItem{line("x", "Cheese Pizza", 1, "Pepperoni"), line("y", "Soda", 3), line("z", "Cheese Pizza", 1, "No Onion")}
	added, removed := DiffLines(before, after)
	if got := Labels(added); !reflect.DeepEqual(got, []string{"Soda", "Cheese Pizza: No Onion"}) {
		t.Fatalf("added %v", got)
	}
	if got := Labels(removed); !reflect.DeepEqual(got, []string{"Wings"}) {
		t.Fatalf("removed %v", got)
	}
	if added[0].Line.Quantity != 1 || added[0].Line.LineTotal != 1000 {
		t.Fatalf("the extra soda: %+v", added[0].Line)
	}
	// Two pieces less of a line, with its option.
	added, removed = DiffLines([]order.LineItem{line("a", "Cheese Pizza", 3, "Pepperoni")}, []order.LineItem{line("b", "Cheese Pizza", 1, "Pepperoni")})
	if len(added) != 0 || len(removed) != 1 || removed[0].Label != "2× Cheese Pizza: Pepperoni" || removed[0].Line.Children[0].Quantity != 2 {
		t.Fatalf("qty down: %+v %+v", added, removed)
	}
	// A note or a portion makes it another line; the same lines = nothing.
	n := line("n", "Soda", 2)
	n.Notes = "no ice"
	if a, r := DiffLines(before[1:2], []order.LineItem{n}); len(a) != 1 || len(r) != 1 {
		t.Fatalf("note: %v %v", a, r)
	}
	if a, r := DiffLines(before, before); len(a) != 0 || len(r) != 0 {
		t.Fatalf("same: %v %v", a, r)
	}
}

func TestKeepLineIDs(t *testing.T) {
	before := []order.LineItem{line("a", "Cheese Pizza", 1, "Pepperoni"), line("b", "Soda", 2)}
	after := []order.LineItem{line("x", "Soda", 2), line("y", "Cheese Pizza", 1, "Pepperoni"), line("z", "Cheese Pizza", 1, "Pepperoni")}
	KeepLineIDs(before, after)
	if after[0].ID != "b" || after[1].ID != "a" || after[1].Children[0].ID != "a-c0" || after[2].ID != "z" {
		t.Fatalf("ids: %s %s %s", after[0].ID, after[1].ID, after[2].ID)
	}
}
