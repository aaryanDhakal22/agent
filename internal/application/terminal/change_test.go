package terminal

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

func TestChangeCartAfterSend(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234") // cashier: discount up to 10 %, refunds up to $10
	soda2 := pricing.Line{Qty: 2, Selection: pricing.Selection{ItemID: soda}}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-chg-00001", Type: "pickup",
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id, before := res.Order.Order.ID, res.Order.Order.Money.Total
	tm, _ := res.Order.Order.SourceMeta["terminal"].(map[string]any)
	if tm["cart"] == nil {
		t.Fatalf("the cart is kept on the order: %v", res.Order.Order.SourceMeta)
	}
	pizzaID := res.Order.Order.Items[0].ID
	// Pay it all, then add two sodas: the balance is the sodas.
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: before}); err != nil {
		t.Fatal(err)
	}
	in := ChangeIn{ClientKey: "chg-000001", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), soda2}}}}
	ch, err := r.svc.ChangeCart(r.ctx, till, id, in)
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Changed || len(ch.Added) != 1 || ch.Added[0] != "2× Soda" || len(ch.Removed) != 0 {
		t.Fatalf("added: %+v", ch)
	}
	if ch.Order.Order.Items[0].ID != pizzaID {
		t.Fatal("the unchanged pizza keeps its line id")
	}
	if ch.BalanceCents != ch.Order.Order.Money.Total-before || ch.Order.Status.Paid || ch.Order.Status.KitchenNumber == nil {
		t.Fatalf("balance: %+v", ch)
	}
	if len(r.orders.addOns) != 1 || len(r.orders.addOns[0]) != 1 || r.orders.addOns[0][0].Quantity != 2 || len(r.orders.voidLns) != 0 {
		t.Fatalf("ADD-ON ticket: %+v", r.orders.addOns)
	}
	if r.pub.count(EventOrderChanged) != 1 {
		t.Fatal("terminal.order.changed")
	}
	// The same key again: the stored answer, nothing printed.
	again, err := r.svc.ChangeCart(r.ctx, till, id, in)
	if err != nil || !again.Duplicate || len(again.Added) != 1 || len(r.orders.addOns) != 1 {
		t.Fatalf("idempotent: %+v %v", again, err)
	}
	// The same cart with no key: nothing changed.
	same, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: in.Cart})
	if err != nil || same.Changed || r.pub.count(EventOrderChanged) != 1 {
		t.Fatalf("no change: %+v %v", same, err)
	}
	// Taking the pizza off would leave less than was paid: refund first.
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{soda2}}}}); !errors.Is(err, terminal.ErrRefundFirst) {
		t.Fatalf("refund first: %v", err)
	}
	// Drop one soda (unpaid part): a VOID ticket for it.
	one := pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: soda}}
	ch, err = r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), one}}}})
	if err != nil || len(ch.Removed) != 1 || ch.Removed[0] != "Soda" || len(r.orders.voidLns) != 1 {
		t.Fatalf("removed: %+v %v", ch, err)
	}
	// A discount over the cashier's limit needs a manager.
	disc := ChangeIn{ClientKey: tk(), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), one, one}}, Discount: &Discount{Pct: 5, Reason: "x"}}}
	disc.Cart.Discount.Pct = 30
	var mr *terminal.ManagerRequired
	if _, err := r.svc.ChangeCart(r.ctx, till, id, disc); !errors.As(err, &mr) {
		t.Fatalf("manager: %v", err)
	}
	// Bad cart, empty cart, unknown order.
	var ci *terminal.CartInvalid
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: 1, Selection: pricing.Selection{ItemID: "nope"}}}}}}); !errors.As(err, &ci) {
		t.Fatalf("cart invalid: %v", err)
	}
	if _, err := r.svc.ChangeCart(r.ctx, till, "0193a000-0000-7000-8000-000000000000", in); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("not found: %v", err)
	}
	// Voided: not editable.
	refundAll(t, r, till, id)
	if _, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "left", ManagerPIN: "9876"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: in.Cart}); !errors.Is(err, terminal.ErrNotEditable) {
		t.Fatalf("voided: %v", err)
	}
	journal, _ := r.repo.Journal(r.ctx, 50)
	n := 0
	for _, e := range journal {
		if e.Action == terminal.ActionOrderChanged {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("journal order.changed: %d", n)
	}
}

// Takeout <-> delivery after SEND (terminal owner pass 2026-11-12): re-quoted for the new type, the
// fulfillment and source_metadata follow, the kitchen gets a change ticket naming the old type,
// journaled; a delivery needs an address; a delivery with a driver is locked; counter never moves.
func TestChangeCartType(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-type-00001", Type: "pickup",
		Customer: &Customer{Name: "Ada Test", Phone: "4105550101"},
		Cart:     Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}
	// No address: refused.
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart, Type: "delivery"}); err == nil {
		t.Fatal("a delivery without an address")
	}
	// Counter is not a type to move to.
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart, Type: "counter"}); err == nil {
		t.Fatal("pickup -> counter")
	}
	addr := &Address{Line1: "1 Test Street", City: "Towson", Zip: "21286"}
	ch, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{Cart: cart, Type: "delivery", ClientKey: "chg-type-0001",
		Customer: &Customer{Name: "Ada Test", Phone: "4105550101", Address: addr}})
	if err != nil {
		t.Fatal(err)
	}
	snap := ch.Order.Order
	if !ch.Changed || !ch.TypeChanged || snap.Fulfillment.Type != "delivery" || snap.Fulfillment.Address == nil || snap.Fulfillment.Address.Line1 != "1 Test Street" {
		t.Fatalf("moved: %+v", ch)
	}
	if orderType(snap) != "delivery" || len(ch.Added) != 0 || len(ch.Removed) != 0 {
		t.Fatalf("type in source_metadata / no line moved: %v %+v", snap.SourceMeta, ch)
	}
	if n := len(r.orders.typeFrom); n == 0 || r.orders.typeFrom[n-1] != "pickup" {
		t.Fatalf("the change ticket names the old type: %v", r.orders.typeFrom)
	}
	if r.pub.count(EventOrderChanged) != 1 {
		t.Fatal("terminal.order.changed")
	}
	journal, _ := r.repo.Journal(r.ctx, 50)
	found := false
	for _, e := range journal {
		if e.Action == terminal.ActionOrderChanged && strings.Contains(string(e.Detail), `"type_to":"delivery"`) && strings.Contains(string(e.Detail), `"type_from":"pickup"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("journaled with type_from / type_to: %+v", journal)
	}
	// Back to a pickup: the address goes; the same type again changes nothing.
	ch, err = r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart, Type: "pickup"})
	if err != nil || !ch.TypeChanged || ch.Order.Order.Fulfillment.Type != "pickup" || ch.Order.Order.Fulfillment.Address != nil {
		t.Fatalf("back to pickup: %+v %v", ch, err)
	}
	same, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart, Type: "pickup"})
	if err != nil || same.Changed || same.TypeChanged {
		t.Fatalf("same type: %+v %v", same, err)
	}
	// A counter order never changes type.
	c, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-type-00002", Type: "counter", Cart: cart})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ChangeCart(r.ctx, till, c.Order.Order.ID, ChangeIn{ClientKey: tk(), Cart: cart, Type: "pickup"}); err == nil {
		t.Fatal("counter -> pickup")
	}
}

func TestTypeChangeRules(t *testing.T) {
	addr := &Customer{Address: &Address{Line1: "1 Test Street", City: "Towson"}}
	for _, c := range []struct {
		cur, to string
		cust    *Customer
		want    string
		bad     bool
	}{
		{"pickup", "", nil, "", false},
		{"pickup", "pickup", nil, "", false},
		{"pickup", "delivery", addr, "delivery", false},
		{"pickup", "delivery", nil, "", true},
		{"pickup", "delivery", &Customer{Address: &Address{Line1: "1 Test Street"}}, "", true},
		{"delivery", "pickup", nil, "pickup", false},
		{"counter", "pickup", nil, "", true},
		{"pickup", "counter", nil, "", true},
	} {
		got, err := typeChange(c.cur, ChangeIn{Type: c.to, Customer: c.cust})
		if got != c.want || (err != nil) != c.bad {
			t.Errorf("%s -> %s: %q %v", c.cur, c.to, got, err)
		}
	}
}

// TERMINAL-FLOWS CHG-14: the customer gave the wrong address after SEND; the order stays a delivery and
// goes to the new one (a name / phone edit too). Nothing prints; main hears order.changed.
func TestChangeKeepsTheTypeAndMovesTheAddress(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-addr-00001", Type: "delivery", Cart: cart,
		Customer: &Customer{Name: "Ada Test", Phone: "4105550101", Address: &Address{Line1: "12 Sample Road", City: "Towson"}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	printed := len(r.orders.typeFrom)
	ch, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart,
		Customer: &Customer{Address: &Address{Line1: "11 Test Street", City: "Towson", Zip: "21286"}}})
	if err != nil || !ch.Changed || ch.TypeChanged || len(ch.Added)+len(ch.Removed) != 0 {
		t.Fatalf("address: %+v %v", ch, err)
	}
	snap := ch.Order.Order
	if snap.Fulfillment.Type != "delivery" || snap.Fulfillment.Address == nil || snap.Fulfillment.Address.Line1 != "11 Test Street" ||
		snap.Fulfillment.Address.PostalCode != "21286" || snap.Customer.DisplayName != "Ada Test" {
		t.Fatalf("moved: %+v %+v", snap.Fulfillment, snap.Customer)
	}
	// (The real PrintChange prints nothing with no line moved and no type band.)
	if n := len(r.orders.typeFrom); n != printed+1 || r.orders.typeFrom[n-1] != "" || r.pub.count(EventOrderChanged) != 1 {
		t.Fatalf("no type band; terminal.order.changed once: %q %d", r.orders.typeFrom, r.pub.count(EventOrderChanged))
	}
	// The same address again (or none given) changes nothing; a name alone does.
	same, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart,
		Customer: &Customer{Address: &Address{Line1: "11 Test Street", City: "Towson", Zip: "21286"}}})
	if err != nil || same.Changed {
		t.Fatalf("same address: %+v %v", same, err)
	}
	if none, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart, Customer: &Customer{}}); err != nil || none.Changed ||
		none.Order.Order.Fulfillment.Address == nil {
		t.Fatalf("nothing given keeps the address: %+v %v", none, err)
	}
	named, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart, Customer: &Customer{Name: "Ada T."}})
	if err != nil || !named.Changed || named.Order.Order.Customer.DisplayName != "Ada T." || named.Order.Order.Fulfillment.Address.Line1 != "11 Test Street" {
		t.Fatalf("name: %+v %v", named, err)
	}
}

// TERMINAL-FLOWS TAB-16: after a TAKE OVER, the old holder's late SEND / SEND Changes carrying the draft is
// refused (open_elsewhere) and changes nothing; the new holder's goes through and removes the draft.
func TestLateSendAfterATakeOverIsRefused(t *testing.T) {
	r := newRig(t)
	till2 := Actor{DeviceID: "dev-2", DeviceName: "Till 2"}
	r.login(t, till, "1234")
	r.login(t, till2, "55")
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}
	// A new order's draft, taken over by till 2.
	if _, err := r.svc.PutDraft(r.ctx, till, "d-late-0001", draftIn(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PutDraft(r.ctx, till2, "d-late-0001", DraftIn{Type: "counter", TakeOver: true, Draft: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var oe *terminal.OpenElsewhere
	_, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-late-00001", Type: "counter", Cart: cart, DraftID: "d-late-0001"})
	if !errors.As(err, &oe) || oe.Holder.DeviceID != till2.DeviceID {
		t.Fatalf("the old holder's SEND: %v", err)
	}
	if list, _ := r.svc.Orders(r.ctx, false); len(list) != 0 {
		t.Fatalf("nothing placed: %d", len(list))
	}
	if _, err := r.svc.PlaceOrder(r.ctx, till2, OrderIn{ClientKey: "k-late-00002", Type: "counter", Cart: cart, DraftID: "d-late-0001"}); err != nil {
		t.Fatal(err)
	}
	if ds, _ := r.svc.Drafts(r.ctx); ds != nil && len(ds.Drafts) != 0 {
		t.Fatalf("the draft went with the SEND: %+v", ds)
	}
	// A sent order open for an edit on till 1, taken over by till 2: till 1's late change is refused.
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-late-00003", Type: "pickup", Cart: cart,
		Customer: &Customer{Name: "Ada Test", Phone: "4105550101"}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	in := draftIn(false)
	in.OrderID = id
	if _, err := r.svc.PutDraft(r.ctx, till, "d-late-0002", in); err != nil {
		t.Fatal(err)
	}
	in.TakeOver = true
	if _, err := r.svc.PutDraft(r.ctx, till2, "d-late-0002", in); err != nil {
		t.Fatal(err)
	}
	two := Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), pizzaLine()}}}
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: two, DraftID: "d-late-0002"}); !errors.As(err, &oe) {
		t.Fatalf("the old holder's change: %v", err)
	}
	if got, _ := r.svc.Order(r.ctx, id); len(got.Order.Order.Items) != 1 {
		t.Fatalf("unchanged: %d lines", len(got.Order.Order.Items))
	}
	if ch, err := r.svc.ChangeCart(r.ctx, till2, id, ChangeIn{ClientKey: tk(), Cart: two, DraftID: "d-late-0002"}); err != nil || !ch.Changed {
		t.Fatalf("the new holder's change: %+v %v", ch, err)
	}
	// A held draft (nobody has it open) may be sent by anyone.
	if _, err := r.svc.PutDraft(r.ctx, till2, "d-late-0003", draftIn(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-late-00004", Type: "counter", Cart: cart, DraftID: "d-late-0003"}); err != nil {
		t.Fatalf("a held draft: %v", err)
	}
}
