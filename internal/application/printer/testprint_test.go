package printer

import (
	"context"
	"testing"
)

func slipFor(e RosterEntry) []byte { return []byte("test:" + e.Name) }

func TestPrintTest_NoFailover(t *testing.T) {
	d, fk := newTestDispatcher(t)
	fk["Pizza"].down = true
	res := d.PrintTest(context.Background(), "", slipFor)
	if len(res) != 3 {
		t.Fatalf("want one result per printer, got %+v", res)
	}
	byName := map[string]TestResult{}
	for _, r := range res {
		byName[r.Name] = r
	}
	if r := byName["Pizza"]; r.OK || r.ErrorCode != TestUnreachable || r.ID != "p-pizza" || r.Error == "" {
		t.Fatalf("pizza: %+v", r)
	}
	if !byName["Front"].OK || !byName["Wings"].OK {
		t.Fatalf("up printers should pass: %+v", res)
	}
	// Each slip printed only on its own printer — the dead one's slip went nowhere.
	if jobs(fk["Front"]) != "test:Front" || jobs(fk["Wings"]) != "test:Wings" || jobs(fk["Pizza"]) != "" {
		t.Fatalf("front=%q wings=%q pizza=%q", jobs(fk["Front"]), jobs(fk["Wings"]), jobs(fk["Pizza"]))
	}
	// Results come back in roster order.
	if res[0].Name != "Front" || res[1].Name != "Pizza" || res[2].Name != "Wings" {
		t.Fatalf("order: %+v", res)
	}
}

func TestPrintTest_OnePrinterAndUnknownID(t *testing.T) {
	d, fk := newTestDispatcher(t)
	fk["Wings"].down = true
	res := d.PrintTest(context.Background(), "p-wings", slipFor)
	if len(res) != 1 || res[0].OK || res[0].ErrorCode != TestUnreachable {
		t.Fatalf("wings: %+v", res)
	}
	if jobs(fk["Front"]) != "" || jobs(fk["Pizza"]) != "" {
		t.Fatal("a single-printer test must not touch the others")
	}
	res = d.PrintTest(context.Background(), "nope", slipFor)
	if len(res) != 1 || res[0].ID != "nope" || res[0].ErrorCode != TestNotConfigured {
		t.Fatalf("unknown id: %+v", res)
	}
}

func TestProbe_DoesNotPrint(t *testing.T) {
	d, fk := newTestDispatcher(t)
	fk["Front"].down = true
	res := d.Probe(context.Background(), "")
	if len(res) != 3 || res[0].Name != "Front" || res[0].OK || res[0].ErrorCode != TestUnreachable || !res[1].OK {
		t.Fatalf("probe: %+v", res)
	}
	for name, f := range fk {
		if jobs(f) != "" {
			t.Fatalf("probe printed on %s", name)
		}
	}
	if st, ok := d.registry.Get("Front"); !ok || st.Up || st.LastChecked == nil {
		t.Fatalf("probe should record the result: %+v", st)
	}
	if got := d.Roster(); len(got) != 3 || !got[0].IsReceipt {
		t.Fatalf("roster: %+v", got)
	}
}
