package order

import (
	"context"
	"testing"
)

func TestUnsettledRules(t *testing.T) {
	mk := func(src, ful string, st State, due int64) StoredOrder {
		return StoredOrder{Order: Snapshot{Source: src, Fulfillment: Fulfillment{Type: ful}}, Status: Status{State: st, CashDueCents: due}}
	}
	counter := mk(SourceCounter, "dine_in", StateReady, 500)
	if !IsUnsettled(counter) || !BlocksClose(counter) || Carries(counter, false) || !Carries(counter, true) {
		t.Fatal("an unpaid counter check blocks a manual close, carried by an auto-close")
	}
	pickup := mk(SourceCounter, "pickup", StateAccepted, 500)
	if BlocksClose(pickup) || !Carries(pickup, false) {
		t.Fatal("an unpaid pickup is carried")
	}
	if IsUnsettled(mk("online", "pickup", StateArrived, 500)) {
		t.Fatal("a ringing order isn't a check")
	}
	if Carries(mk(SourceCounter, "delivery", StateOnDelivery, 500), true) {
		t.Fatal("a delivery is never carried")
	}
	if IsUnsettled(mk(SourceCounter, "delivery", StateDelivered, 500)) {
		t.Fatal("a delivered delivery's cash is the driver's")
	}
	paid := mk(SourceCounter, "pickup", StateReady, 0)
	if IsUnsettled(paid) || Settled(paid) {
		t.Fatal("paid but not picked up: not unsettled, not settled yet")
	}
}

func TestUnpaidAtCloseRules(t *testing.T) {
	if UnpaidActionOf("") != UnpaidWaste || UnpaidActionOf("Carry ") != UnpaidCarry || UnpaidActionOf("nonsense") != UnpaidWaste {
		t.Fatal("the setting: carry, else waste")
	}
	if ForceOutcome("waste", 0) != UnpaidWaste || ForceOutcome("waste", 1) != UnpaidCarry || ForceOutcome("carry", 0) != UnpaidCarry {
		t.Fatal("waste only with no payment row; a partly paid check is carried")
	}
	if CloseUnpaidOf(context.Background()) != UnpaidWaste || CloseUnpaidOf(WithCloseUnpaid(context.Background(), "carry")) != UnpaidCarry {
		t.Fatal("ctx: waste by default")
	}
	a := CloseActorOf(WithCloseKind(context.Background(), CloseAuto))
	if a.Trigger != "auto" || a.DeviceID != "pi" {
		t.Fatalf("auto actor: %+v", a)
	}
	e := &UnsettledError{Checks: make([]UnsettledCheck, 2), ForceAction: UnpaidWaste, Force: ForcePreview{WasteCount: 1, WasteCents: 1299, CarryCount: 1}}
	if e.Error() != "2 unpaid counter check(s) are open: force close writes off 1 as waste ($12.99) and carries 1 over" {
		t.Fatal(e.Error())
	}
}
