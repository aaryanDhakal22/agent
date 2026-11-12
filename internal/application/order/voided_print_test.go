package order

import (
	"testing"

	"quiccpos/agent/internal/domain/order"
)

// A reprint of a voided / cancelled order prints it struck through under a
// banner (receipt.writeVoided): printable names it.
func TestPrintableMarksAVoidedOrder(t *testing.T) {
	so := order.StoredOrder{Order: order.Snapshot{ID: "o1", Source: order.SourceCounter}}
	if printable(so).Voided != "" {
		t.Fatal("an open order isn't voided")
	}
	so.Status.State = order.StateCancelled
	if got := printable(so).Voided; got != "VOID" {
		t.Fatalf("voided counter order: %q", got)
	}
	so.Order.Source = "online"
	if got := printable(so).Voided; got != "CANCELLED" {
		t.Fatalf("cancelled online order: %q", got)
	}
}
