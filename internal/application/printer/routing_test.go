package printer

import (
	"context"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/printroute"
	"quiccpos/agent/internal/domain/storeconfig"
)

// newRoutingDispatcher is newTestDispatcher with a fourth printer, Spare.
func newRoutingDispatcher(t *testing.T) (*Dispatcher, map[string]*fakePrinter) {
	t.Helper()
	d, fk := newTestDispatcher(t)
	d.Apply(context.Background(), []storeconfig.PrinterContext{
		{ID: "p-front", Name: "Front", IP: "x", IsReceipt: true},
		{ID: "p-pizza", Name: "Pizza", IP: "x"},
		{ID: "p-wings", Name: "Wings", IP: "x"},
		{ID: "p-spare", Name: "Spare", IP: "x"},
	})
	return d, fk
}

// settleProbes waits (polling) for the health check's first probe of every
// printer, so a test's own registry.Record isn't overwritten by it.
func settleProbes(t *testing.T, d *Dispatcher) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, e := range d.Roster() {
		for {
			if st, ok := d.registry.Get(e.Name); ok && st.LastChecked != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never probed", e.Name)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestRouting_NoneKeepsTheAutomaticOrder(t *testing.T) {
	d, fk := newRoutingDispatcher(t)
	fk["Wings"].setDown(true)
	out, err := d.Execute(context.Background(), routedSnap())
	if err != nil {
		t.Fatal(err)
	}
	if jobs(fk["Front"]) != "station:Wings:rerouted=Wings,receipt:Front" || jobs(fk["Spare"]) != "" {
		t.Fatalf("front=%q spare=%q", jobs(fk["Front"]), jobs(fk["Spare"]))
	}
	for _, p := range out.Printed {
		if p.Station == "Wings" && p.RerouteBy != "auto" {
			t.Fatalf("by = %q", p.RerouteBy)
		}
	}
}

func TestRouting_OneEmergencyPrinterForAll(t *testing.T) {
	d, fk := newRoutingDispatcher(t)
	if _, err := d.SaveRouting(context.Background(), printroute.Routing{Mode: printroute.ModeOne, EmergencyPrinterID: "p-spare"}); err != nil {
		t.Fatal(err)
	}
	fk["Wings"].setDown(true)
	fk["Front"].setDown(true)
	out, err := d.Execute(context.Background(), routedSnap())
	if err != nil {
		t.Fatal(err)
	}
	// Both the Wings ticket and the Front receipt go to Spare (not the
	// receipt printer / the first station, as the automatic order would).
	if jobs(fk["Spare"]) != "station:Wings:rerouted=Wings,receipt:Front:rerouted=Front" || jobs(fk["Pizza"]) != "station:Pizza" {
		t.Fatalf("spare=%q pizza=%q", jobs(fk["Spare"]), jobs(fk["Pizza"]))
	}
	for _, p := range out.Printed {
		if p.ReroutedFrom != "" && (p.PrintedOn != "Spare" || p.RerouteBy != "rule") {
			t.Fatalf("result %+v", p)
		}
	}
}

func TestRouting_PerPrinterTargetDownGoesToTheNextRule(t *testing.T) {
	d, fk := newRoutingDispatcher(t)
	if _, err := d.SaveRouting(context.Background(), printroute.Routing{Mode: printroute.ModePerPrinter,
		Targets: map[string][]string{"p-wings": {"p-spare", "p-pizza"}}}); err != nil {
		t.Fatal(err)
	}
	fk["Wings"].setDown(true)
	fk["Spare"].setDown(true) // the first stand-in is down too (not yet seen by the health check)
	out, err := d.Execute(context.Background(), routedSnap())
	if err != nil {
		t.Fatal(err)
	}
	if jobs(fk["Pizza"]) != "station:Pizza,station:Wings:rerouted=Wings" || jobs(fk["Front"]) != "receipt:Front" {
		t.Fatalf("pizza=%q front=%q", jobs(fk["Pizza"]), jobs(fk["Front"]))
	}
	if detects(fk["Spare"]) == 0 {
		t.Fatal("the first rule target should be tried first")
	}
	for _, p := range out.Printed {
		if p.Station == "Wings" && (p.PrintedOn != "Pizza" || p.RerouteBy != "rule") {
			t.Fatalf("wings result %+v", p)
		}
	}
}

func TestRouting_EveryRuleTargetDownFallsBackToAutomatic(t *testing.T) {
	d, fk := newRoutingDispatcher(t)
	if _, err := d.SaveRouting(context.Background(), printroute.Routing{Mode: printroute.ModePerPrinter,
		Targets: map[string][]string{"p-wings": {"p-spare"}}}); err != nil {
		t.Fatal(err)
	}
	fk["Wings"].setDown(true)
	fk["Spare"].setDown(true)
	out, err := d.Execute(context.Background(), routedSnap())
	if err != nil {
		t.Fatal(err)
	}
	if jobs(fk["Front"]) != "station:Wings:rerouted=Wings,receipt:Front" {
		t.Fatalf("front=%q", jobs(fk["Front"]))
	}
	for _, p := range out.Printed {
		if p.Station == "Wings" && p.RerouteBy != "auto" {
			t.Fatalf("wings result %+v", p)
		}
	}
}

func TestRouting_KnownDownRuleTargetIsTriedAfterTheUpOnes(t *testing.T) {
	d, fk := newRoutingDispatcher(t)
	if _, err := d.SaveRouting(context.Background(), printroute.Routing{Mode: printroute.ModeOne, EmergencyPrinterID: "p-spare"}); err != nil {
		t.Fatal(err)
	}
	settleProbes(t, d)
	fk["Wings"].setDown(true)
	fk["Spare"].setDown(true)
	d.registry.Record("Spare", "x", false, errors.New("refused"), time.Now())
	if _, err := d.Execute(context.Background(), routedSnap()); err != nil {
		t.Fatal(err)
	}
	// A stand-in the health check saw down goes behind every up printer.
	cands, rule := d.candidates(d.byID["p-wings"], d.roster)
	names := ""
	for _, c := range cands {
		names += c.entry.Name + " "
	}
	if names != "Wings Front Pizza Spare " || !rule["p-spare"] {
		t.Fatalf("order = %q rule=%v", names, rule)
	}
	if jobs(fk["Front"]) != "station:Wings:rerouted=Wings,receipt:Front" {
		t.Fatalf("front=%q", jobs(fk["Front"]))
	}
}

func TestRouting_DocumentsFollowTheReceiptPrintersRule(t *testing.T) {
	d, fk := newRoutingDispatcher(t)
	if _, err := d.SaveRouting(context.Background(), printroute.Routing{Mode: printroute.ModePerPrinter,
		Targets: map[string][]string{"p-front": {"p-wings"}}}); err != nil {
		t.Fatal(err)
	}
	fk["Front"].setDown(true)
	res, err := d.PrintDocument(context.Background(), "driver-report", func(from string) []byte { return []byte("report:" + from) })
	if err != nil || res.PrintedOn != "Wings" || res.RerouteBy != "rule" || jobs(fk["Wings"]) != "report:Front" {
		t.Fatalf("res=%+v err=%v wings=%q", res, err, jobs(fk["Wings"]))
	}
}

func TestRouting_SaveValidatesAgainstTheRoster(t *testing.T) {
	d, _ := newRoutingDispatcher(t)
	_, err := d.SaveRouting(context.Background(), printroute.Routing{Mode: printroute.ModeOne, EmergencyPrinterID: "p-gone"})
	var ve *printroute.ValidationError
	if !errors.As(err, &ve) || ve.Code != "unknown_printer" {
		t.Fatalf("err = %v", err)
	}
	if d.Routing().Mode != printroute.ModeNone {
		t.Fatal("a refused setting must not apply")
	}
	// A printer removed from the roster later is skipped at print time.
	d.SetRouting(printroute.Routing{Mode: printroute.ModeOne, EmergencyPrinterID: "p-gone"})
	cands, _ := d.candidates(d.roster[2], d.roster)
	if len(cands) != 4 || cands[1].entry.ID != "p-front" {
		t.Fatalf("cands = %v", cands)
	}
}

type memRoutingStore struct {
	r     printroute.Routing
	saves int
}

func (m *memRoutingStore) PrintRouting(context.Context) (printroute.Routing, error) { return m.r, nil }
func (m *memRoutingStore) SetPrintRouting(_ context.Context, r printroute.Routing) error {
	m.r, m.saves = r, m.saves+1
	return nil
}

func TestRouting_StoreLoadSaveAndChanged(t *testing.T) {
	d, _ := newRoutingDispatcher(t)
	st := &memRoutingStore{r: printroute.Routing{Mode: printroute.ModeOne, EmergencyPrinterID: "p-spare"}}
	woke := 0
	d.SetRoutingStore(st, func() { woke++ })
	d.LoadRouting(context.Background())
	if d.Routing().EmergencyPrinterID != "p-spare" {
		t.Fatalf("loaded %+v", d.Routing())
	}
	if _, err := d.SaveRouting(context.Background(), printroute.Routing{Mode: printroute.ModeNone}); err != nil {
		t.Fatal(err)
	}
	if st.saves != 1 || woke != 1 || d.Routing().Mode != printroute.ModeNone {
		t.Fatalf("saves=%d woke=%d r=%+v", st.saves, woke, d.Routing())
	}
}

func TestRoutingView_StatusAndFallbackOrder(t *testing.T) {
	d, _ := newRoutingDispatcher(t)
	d.SetRouting(printroute.Routing{Mode: printroute.ModePerPrinter, Targets: map[string][]string{"p-wings": {"p-spare"}}})
	settleProbes(t, d)
	now := time.Now()
	d.registry.Record("Front", "x", true, nil, now)
	d.registry.Record("Spare", "x", false, errors.New("refused"), now)
	view := d.RoutingView()
	if len(view) != 4 {
		t.Fatalf("view = %+v", view)
	}
	var wings RoutingPrinter
	for _, p := range view {
		if p.ID == "p-wings" {
			wings = p
		}
		if p.ID == "p-front" && p.Status != "up" {
			t.Fatalf("front status %q", p.Status)
		}
	}
	// Spare is the rule target but down: tried after the up ones.
	got := ""
	for _, f := range wings.Fallbacks {
		got += f.Name + "/" + f.By + "/" + f.Status + " "
	}
	if got != "Front/auto/up Pizza/auto/up Spare/rule/down " {
		t.Fatalf("wings fallbacks = %q", got)
	}
}
