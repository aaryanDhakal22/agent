package receipt

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

func unsettledFixture() order.UnsettledList {
	placed := time.Date(2026, 11, 12, 23, 15, 0, 0, time.UTC)
	return order.UnsettledList{BatchID: 41, Count: 2, DueCents: 3656, Checks: []order.UnsettledCheck{
		{OrderID: "a", Number: 12, Type: "counter", State: order.StateReady, PlacedAt: placed, BatchID: 41, Carried: true,
			StaffName: "Cara S.", TotalCents: 2357, PaidCents: 1000, DueCents: 1357,
			Items: []order.UnsettledLineItem{{Qty: 1, Name: "Cheese Pizza (16 XL)", Mods: []string{"Pepperoni (1st half)"}, PriceCents: 2199}}},
		{OrderID: "b", Number: 1107, Type: "pickup", Source: "web", State: order.StateReady, PlacedAt: placed.Add(20 * time.Minute), BatchID: 41,
			Carried: true, Customer: order.UnsettledCustomer{Name: "Sam Lee", Phone: "4105550100"}, TotalCents: 2299, DueCents: 2299,
			Items: []order.UnsettledLineItem{{Qty: 2, Name: "Garlic Knots", Mods: []string{}, PriceCents: 2299}}},
	}}
}

func TestBuildUnsettledList_Golden(t *testing.T) {
	sc := storeconfig.StoreContext{Location: storeconfig.LocationContext{Name: "Lombardi's Pizza", Timezone: "America/New_York"}}
	got := BuildUnsettledList(unsettledFixture(), sc, "", printer.Paper{}, time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC))
	path := "testdata/unsettled_list.golden"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("unsettled list differs from %s (run with -update after checking)\n%s", path, got)
	}
	text := string(got)
	for _, s := range []string{"UNSETTLED", "#12 COUNTER (KEPT OPEN)", "#1107 PICKUP", "Sam Lee 4105550100", "Cara S.",
		"Pepperoni (1st half)", "$13.57", "$22.99", "$36.56"} {
		if !strings.Contains(text, s) {
			t.Errorf("missing %q", s)
		}
	}
}

func TestBuildUnpaidAtClose_Golden(t *testing.T) {
	sc := storeconfig.StoreContext{Location: storeconfig.LocationContext{Name: "Lombardi's Pizza", Timezone: "America/New_York"}}
	l := unsettledFixture()
	at := time.Date(2026, 11, 13, 6, 30, 0, 0, time.UTC)
	c := l.Checks[0]
	c.Carried, c.PaidCents, c.DueCents, c.TotalCents = false, 0, 2357, 2357
	u := order.UnpaidAtClose{BatchID: 41, Reason: order.UnpaidAtCloseReason, At: at, Count: 1, TotalCents: 2357, PrintedBy: "pi",
		ClosedBy: order.CloseActor{StaffName: "Mia M.", DeviceID: "t1", Trigger: "terminal"}, Checks: []order.UnsettledCheck{c}}
	got := BuildUnpaidAtClose(u, sc, "", printer.Paper{}, at)
	path := "testdata/unpaid_at_close.golden"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("unpaid at close differs from %s (run with -update after checking)\n%s", path, got)
	}
	text := string(got)
	for _, s := range []string{"UNPAID", "AT CLOSE", "Written off as waste: Unpaid at close", "#12 COUNTER", "Cara S.",
		"Pepperoni (1st half)", "$23.57", "Closed by Mia M.", "Checks"} {
		if !strings.Contains(text, s) {
			t.Errorf("missing %q", s)
		}
	}
	u.Auto = true
	if !strings.Contains(string(BuildUnpaidAtClose(u, sc, "", printer.Paper{}, at)), "Closed by the auto-close") {
		t.Error("auto-close: who closed")
	}
}
