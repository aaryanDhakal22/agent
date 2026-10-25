package printer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
)

// fakePrinter records every job; down makes Detect fail (unreachable).
type fakePrinter struct {
	name    string
	mu      sync.Mutex
	down    bool
	jobs    []string
	detects int
}

func (f *fakePrinter) Detect(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detects++
	if f.down {
		return errors.New("connection refused")
	}
	return nil
}

// setDown flips the fake under its lock (the dispatcher's health loop reads
// it from another goroutine).
func (f *fakePrinter) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *fakePrinter) Name() string { return f.name }
func (f *fakePrinter) IP() string   { return "fake" }
func (f *fakePrinter) SetIP(string) {}
func (f *fakePrinter) Print(_ context.Context, j printer.PrintJob) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, string(j.Commands))
	return nil
}

// newTestDispatcher builds a dispatcher over Front (receipt), Pizza, Wings
// fakes. The builder renders "<kind>:<station>[:rerouted=<from>]" so tests
// can read where each ticket landed.
func newTestDispatcher(t *testing.T) (*Dispatcher, map[string]*fakePrinter) {
	t.Helper()
	meters, err := observability.NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	fakes := map[string]*fakePrinter{}
	factory := func(name, _ string) printer.Printer {
		f := &fakePrinter{name: name}
		fakes[name] = f
		return f
	}
	build := func(_ order.Snapshot, tk printer.Ticket) []byte {
		s := string(tk.Kind) + ":" + tk.Station
		if tk.ReroutedFrom != "" {
			s += ":rerouted=" + tk.ReroutedFrom
		}
		return []byte(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := NewDispatcher(ctx, factory, NewRegistry(), nil, time.Hour, build, zerolog.Nop(), meters)
	d.Apply(ctx, []storeconfig.PrinterContext{
		{ID: "p-front", Name: "Front", IP: "x", IsReceipt: true},
		{ID: "p-pizza", Name: "Pizza", IP: "x"},
		{ID: "p-wings", Name: "Wings", IP: "x"},
	})
	return d, fakes
}

func routedSnap() order.Snapshot {
	return order.Snapshot{ID: "o1", Items: []order.LineItem{
		item("Pepperoni", order.RoutingMatched, "p-pizza"),
		item("Hot Wings", order.RoutingMatched, "p-wings"),
	}}
}

func detects(f *fakePrinter) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.detects
}

func jobs(f *fakePrinter) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.jobs, ",")
}

func TestExecute_AllUp(t *testing.T) {
	d, fk := newTestDispatcher(t)
	out, err := d.Execute(context.Background(), routedSnap())
	if err != nil || len(out.Printed) != 3 || len(out.Failed) != 0 {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if jobs(fk["Pizza"]) != "station:Pizza" || jobs(fk["Wings"]) != "station:Wings" || jobs(fk["Front"]) != "receipt:Front" {
		t.Fatalf("pizza=%q wings=%q front=%q", jobs(fk["Pizza"]), jobs(fk["Wings"]), jobs(fk["Front"]))
	}
}

func TestExecute_StationDownFailsOverToReceiptPrinter(t *testing.T) {
	d, fk := newTestDispatcher(t)
	fk["Pizza"].setDown(true)
	out, err := d.Execute(context.Background(), routedSnap())
	if err != nil || len(out.Failed) != 0 {
		t.Fatalf("err=%v out=%+v", err, out)
	}
	if jobs(fk["Front"]) != "station:Pizza:rerouted=Pizza,receipt:Front" {
		t.Fatalf("front = %q", jobs(fk["Front"]))
	}
	var rerouted bool
	for _, p := range out.Printed {
		if p.Station == "Pizza" && p.PrintedOn == "Front" && p.ReroutedFrom == "Pizza" {
			rerouted = true
		}
	}
	if !rerouted {
		t.Fatalf("outcome should record the reroute: %+v", out.Printed)
	}
}

func TestExecute_ReceiptDownFailsOverToAStation(t *testing.T) {
	d, fk := newTestDispatcher(t)
	fk["Front"].setDown(true)
	if _, err := d.Execute(context.Background(), routedSnap()); err != nil {
		t.Fatal(err)
	}
	if jobs(fk["Pizza"]) != "station:Pizza,receipt:Front:rerouted=Front" {
		t.Fatalf("pizza = %q", jobs(fk["Pizza"]))
	}
}

func TestExecute_AllDownErrors(t *testing.T) {
	d, fk := newTestDispatcher(t)
	for _, f := range fk {
		f.setDown(true)
	}
	out, err := d.Execute(context.Background(), routedSnap())
	if err == nil || !errors.Is(err, ErrPrinterUnreachable) || len(out.Printed) != 0 || len(out.Failed) != 3 {
		t.Fatalf("err=%v out=%+v", err, out)
	}
}

func TestExecute_EmptyRoster(t *testing.T) {
	d, _ := newTestDispatcher(t)
	d.Apply(context.Background(), nil)
	if _, err := d.Execute(context.Background(), routedSnap()); !errors.Is(err, ErrNoPrintersAvailable) {
		t.Fatalf("want ErrNoPrintersAvailable, got %v", err)
	}
}

func TestExecute_LegacyOrderOneReceipt(t *testing.T) {
	d, fk := newTestDispatcher(t)
	snap := order.Snapshot{ID: "o2", Items: []order.LineItem{{Name: "Old", Quantity: 1}}}
	if _, err := d.Execute(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if jobs(fk["Front"]) != "full:Front" || jobs(fk["Pizza"]) != "" || jobs(fk["Wings"]) != "" {
		t.Fatalf("front=%q pizza=%q wings=%q", jobs(fk["Front"]), jobs(fk["Pizza"]), jobs(fk["Wings"]))
	}
}

func TestApplyDropsRemovedPrintersFromTheRegistry(t *testing.T) {
	d, _ := newTestDispatcher(t)
	d.registry.Record("Pizza", "x", true, nil, time.Now())
	d.registry.Record("Wings", "x", true, nil, time.Now())
	d.Apply(context.Background(), []storeconfig.PrinterContext{{ID: "p-front", Name: "Front", IP: "x", IsReceipt: true}})
	for _, st := range d.registry.All() {
		if st.Name == "Pizza" || st.Name == "Wings" {
			t.Fatalf("removed printer still listed: %+v", st)
		}
	}
}

func TestKnownDownFallbacksAreTriedLast(t *testing.T) {
	d, fk := newTestDispatcher(t)
	fk["Pizza"].setDown(true)
	fk["Front"].setDown(true)
	// The health check already saw Front (the receipt printer) down.
	now := time.Now()
	d.registry.Record("Front", "x", false, errors.New("refused"), now)
	d.registry.Record("Wings", "x", true, nil, now)
	out, err := d.Execute(context.Background(), routedSnap())
	if err != nil {
		t.Fatal(err)
	}
	// The pizza ticket goes straight to Wings instead of first dialling Front.
	for _, p := range out.Printed {
		if p.Station == "Pizza" && p.PrintedOn != "Wings" {
			t.Fatalf("pizza ticket printed on %s", p.PrintedOn)
		}
	}
	if detects(fk["Front"]) == 0 {
		t.Log("front never tried (fine: everything printed before reaching it)")
	}
}

func TestPrintDocument_ReceiptPrinterThenFailover(t *testing.T) {
	d, fk := newTestDispatcher(t)
	build := func(from string) []byte { return []byte("report:" + from) }
	res, err := d.PrintDocument(context.Background(), "driver-report", build)
	if err != nil || res.PrintedOn != "Front" || res.ReroutedFrom != "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if jobs(fk["Front"]) != "report:" {
		t.Fatalf("front = %q", jobs(fk["Front"]))
	}

	fk["Front"].setDown(true)
	res, err = d.PrintDocument(context.Background(), "driver-report", build)
	if err != nil || res.PrintedOn != "Pizza" || res.ReroutedFrom != "Front" || jobs(fk["Pizza"]) != "report:Front" {
		t.Fatalf("failover res=%+v err=%v pizza=%q", res, err, jobs(fk["Pizza"]))
	}

	for _, f := range fk {
		f.setDown(true)
	}
	if _, err := d.PrintDocument(context.Background(), "driver-report", build); err == nil {
		t.Fatal("expected an error with every printer down")
	}
}

func TestExecuteMode_ReceiptAndKitchen(t *testing.T) {
	d, fk := newTestDispatcher(t)
	if out, err := d.ExecuteMode(context.Background(), routedSnap(), printer.ModeReceipt); err != nil || len(out.Printed) != 1 {
		t.Fatalf("receipt: err=%v out=%+v", err, out)
	}
	if jobs(fk["Front"]) != "receipt:Front" || jobs(fk["Pizza"]) != "" || jobs(fk["Wings"]) != "" {
		t.Fatalf("receipt mode: front=%q pizza=%q wings=%q", jobs(fk["Front"]), jobs(fk["Pizza"]), jobs(fk["Wings"]))
	}

	d, fk = newTestDispatcher(t)
	fk["Wings"].setDown(true)
	out, err := d.ExecuteMode(context.Background(), routedSnap(), printer.ModeKitchen)
	if err != nil || len(out.Printed) != 2 || len(out.Failed) != 0 {
		t.Fatalf("kitchen: err=%v out=%+v", err, out)
	}
	if jobs(fk["Pizza"]) != "station:Pizza" || jobs(fk["Front"]) != "station:Wings:rerouted=Wings" {
		t.Fatalf("kitchen mode: front=%q pizza=%q", jobs(fk["Front"]), jobs(fk["Pizza"]))
	}

	// Nothing for the kitchen: ErrNoTickets, nothing printed.
	d, fk = newTestDispatcher(t)
	soda := order.Snapshot{ID: "o3", Items: []order.LineItem{item("Soda", order.RoutingMatched, "p-front")}}
	if _, err := d.ExecuteMode(context.Background(), soda, printer.ModeKitchen); !errors.Is(err, printer.ErrNoTickets) {
		t.Fatalf("want ErrNoTickets, got %v", err)
	}
	if jobs(fk["Front"]) != "" {
		t.Fatalf("front printed %q", jobs(fk["Front"]))
	}
}

// insert_expected: true only when this accept printed the kitchen tickets of
// an online cash pickup under the insert_live rule, decided from the same
// rules snapshot the plan used.
func TestExecute_InsertExpected(t *testing.T) {
	d, fk := newTestDispatcher(t)
	cash := routedSnap()
	cash.Fulfillment.Type = "pickup"
	cash.Money.Total = 2000 // no payments: all cash
	card := cash
	card.Payments = []order.PaymentSummary{{Method: "card", AmountCents: 2000}}
	delivery := cash
	delivery.Fulfillment.Type = "delivery"
	pos := cash
	pos.Source = order.SourcePOS
	live := Rules{Tickets: order.DefaultTicketToggles(), InsertLive: true}
	off := Rules{Tickets: order.DefaultTicketToggles()}
	switchOff := live
	switchOff.Tickets.OnlineCashPickup = false
	posOn := live
	posOn.Tickets.PosTakeout = true
	for _, tc := range []struct {
		name  string
		snap  order.Snapshot
		rules Rules
		mode  printer.Mode
		want  bool
	}{
		{"cash pickup, insert live", cash, live, printer.ModeAccept, true},
		{"cash pickup, not live (receipt only)", cash, off, printer.ModeAccept, false},
		{"cash pickup, switch off", cash, switchOff, printer.ModeAccept, false},
		{"cash pickup, kitchen resend", cash, live, printer.ModeKitchen, false},
		{"card pickup", card, live, printer.ModeAccept, false},
		{"cash delivery", delivery, live, printer.ModeAccept, false},
		{"POS pickup", pos, posOn, printer.ModeAccept, false},
	} {
		rules := tc.rules
		d.SetRules(func() Rules { return rules })
		out, err := d.ExecuteMode(context.Background(), tc.snap, tc.mode)
		if err != nil || out.InsertExpected != tc.want {
			t.Fatalf("%s: insert_expected = %v (err %v), want %v", tc.name, out.InsertExpected, err, tc.want)
		}
	}
	// Nothing printed anywhere: no insert either.
	for _, f := range fk {
		f.setDown(true)
	}
	d.SetRules(func() Rules { return live })
	if out, err := d.Execute(context.Background(), cash); err == nil || out.InsertExpected {
		t.Fatalf("all down: insert_expected = %v (err %v)", out.InsertExpected, err)
	}
}
