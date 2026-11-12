package order

import (
	"context"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
)

// The arrival stamp (show_on_driver_ticket, terminal.Store.StampDriverTicket in
// cmd/main.go) runs before an order from main is stored, and its stamps are
// in the stored snapshot.
func TestOnArrival_StampsBeforeStoring(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newWebService(t)
	calls := 0
	svc.SetArrivalStamp(func(s *order.Snapshot) {
		calls++
		f := false
		s.Items[0].Children[0].ShowOnDriverTicket = &f
	})
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "web",
		SourceEventID: "w-stamp", PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: "delivery"},
		Items: []order.LineItem{{Name: "Combo", Quantity: 1, MenuItemID: uuid.NewString(),
			Children: []order.LineItem{{Name: "Well Done", Quantity: 1}}}},
		Money: order.Money{Total: 1299}}
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	so, err := svc.repo.GetByID(ctx, uuid.MustParse(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	got := so.Order.Items[0].Children[0].ShowOnDriverTicket
	if calls != 1 || got == nil || *got {
		t.Fatalf("stamp calls %d, stored %v", calls, got)
	}
}
