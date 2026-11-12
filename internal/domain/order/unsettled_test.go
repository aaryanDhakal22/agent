package order

import "testing"

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
