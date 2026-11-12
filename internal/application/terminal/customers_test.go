package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// The store's customers on the Pi (terminal 1d, contract §2.10j): main's
// feed (pages, tombstones, reset, another store), lookup by partial digits,
// the area check, terminal writes (customer.upsert in the outbox), the
// customer's orders. Synthetic data only (555 numbers, "Test" names).

// feedMain serves main's customer feed: queued pages, the since= it was asked.
type feedMain struct {
	mu     sync.Mutex
	pages  []any
	asked  []string
	status int
}

func (f *feedMain) fetch(_ context.Context, path, rawQuery string) (int, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, _ := url.ParseQuery(rawQuery)
	f.asked = append(f.asked, q.Get("since"))
	if f.status != 0 {
		return f.status, nil, nil
	}
	if path != PathCustomers || len(f.pages) == 0 {
		b, _ := json.Marshal(map[string]any{"customers": []any{}, "deleted": []any{}, "cursor": "c-end", "has_more": false, "reset": false})
		return 200, b, nil
	}
	p := f.pages[0]
	f.pages = f.pages[1:]
	b, _ := json.Marshal(p)
	return 200, b, nil
}

func feedCust(id, phone, first string, at time.Time, addrs ...map[string]any) map[string]any {
	if addrs == nil {
		addrs = []map[string]any{}
	}
	return map[string]any{"id": id, "phone": phone, "first_name": first, "last_name": "Test", "name": first + " Test", "notes": "",
		"source": "import", "updated_at": at, "addresses": addrs}
}

func page(cursor string, more bool, deleted []string, cs ...map[string]any) map[string]any {
	if deleted == nil {
		deleted = []string{}
	}
	if cs == nil {
		cs = []map[string]any{}
	}
	return map[string]any{"customers": cs, "deleted": deleted, "cursor": cursor, "has_more": more, "reset": false}
}

type custRig struct {
	*rig
	feed *feedMain
	book *CustomerBook
	loc  string
	lat  *float64
	lng  *float64
	rad  int64
}

func newCustRig(t *testing.T) *custRig {
	r := &custRig{rig: newRig(t), feed: &feedMain{}, loc: "loc-1"}
	r.book = NewCustomerBook(r.repo, r.feed.fetch, r.pub, func() string { return r.loc },
		func() (*float64, *float64, int64) { return r.lat, r.lng, r.rad }, zerolog.Nop())
	r.svc.SetCustomers(r.book)
	return r
}

func (r *custRig) phones(t *testing.T, digits string) []string {
	t.Helper()
	l, err := r.svc.LookupCustomers(r.ctx, digits)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, c := range l.Customers {
		out = append(out, c.Phone)
	}
	return out
}

func TestCustomerFeedPagesTombstonesResetAndAnotherStore(t *testing.T) {
	r := newCustRig(t)
	t0 := time.Now().UTC().Add(-time.Hour)
	a, b, c := uuid.NewString(), uuid.NewString(), uuid.NewString()
	r.feed.pages = []any{
		page("c1", true, nil, feedCust(a, "4105550101", "Ada", t0, map[string]any{"id": uuid.NewString(), "line1": "1 Test St", "city": "Towson", "zip": "21286", "default": true})),
		page("c2", false, nil, feedCust(b, "4105550102", "Bo", t0), feedCust(c, "4435550103", "Cy", t0)),
	}
	if err := r.book.Pull(r.ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.repo.CountCustomers(r.ctx); n != 3 {
		t.Fatalf("count = %d", n)
	}
	if r.feed.asked[0] != "" || r.feed.asked[1] != "c1" {
		t.Fatalf("since asked: %v", r.feed.asked)
	}
	got, _ := r.repo.Customer(r.ctx, a)
	if got.Name != "Ada Test" || len(got.Addresses) != 1 || got.Addresses[0].Line1 != "1 Test St" || got.Pending {
		t.Fatalf("ada: %+v", got)
	}

	// The next pull starts at the saved cursor: B deleted, A renamed.
	r.feed.asked = nil
	r.feed.pages = []any{page("c3", false, []string{b}, feedCust(a, "4105550101", "Adah", t0.Add(time.Minute)))}
	if err := r.book.Pull(r.ctx); err != nil {
		t.Fatal(err)
	}
	if r.feed.asked[0] != "c2" {
		t.Fatalf("since = %v", r.feed.asked)
	}
	if _, err := r.repo.Customer(r.ctx, b); !errors.Is(err, terminal.ErrCustomerNotFound) {
		t.Fatalf("tombstone: %v", err)
	}
	if got, _ := r.repo.Customer(r.ctx, a); got.Name != "Adah Test" || len(got.Addresses) != 0 {
		t.Fatalf("renamed: %+v", got)
	}

	// main resets: the copy starts over from an empty cursor.
	d := uuid.NewString()
	r.feed.asked = nil
	r.feed.pages = []any{map[string]any{"customers": []any{}, "deleted": []any{}, "cursor": "", "has_more": false, "reset": true},
		page("c9", false, nil, feedCust(d, "4105550199", "Di", t0))}
	if err := r.book.Pull(r.ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.feed.asked) != 2 || r.feed.asked[1] != "" {
		t.Fatalf("after reset, since asked: %v", r.feed.asked)
	}
	if n, _ := r.repo.CountCustomers(r.ctx); n != 1 {
		t.Fatalf("after reset count = %d", n)
	}

	// Linked to another store: everything goes, from the start.
	r.loc = "loc-2"
	r.feed.asked = nil
	if err := r.book.Pull(r.ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.repo.CountCustomers(r.ctx); n != 0 || r.feed.asked[0] != "" {
		t.Fatalf("another store: %d %v", n, r.feed.asked)
	}

	// An older main without the feed.
	r.feed.status = 404
	if err := r.book.Pull(r.ctx); !errors.Is(err, ErrMainUnsupported) {
		t.Fatalf("404: %v", err)
	}
}

func TestCustomerLookupByPartialDigitsAndArea(t *testing.T) {
	r := newCustRig(t)
	t0 := time.Now().UTC().Add(-time.Hour)
	near, far := uuid.NewString(), uuid.NewString()
	r.feed.pages = []any{page("c1", false, nil,
		feedCust(uuid.NewString(), "4105550101", "Ada", t0, map[string]any{"id": near, "line1": "1 Test St", "city": "Towson"},
			map[string]any{"id": far, "line1": "9 Far Rd", "city": "Elsewhere"},
			map[string]any{"id": uuid.NewString(), "line1": "5 New Rd", "city": "Towson"}),
		feedCust(uuid.NewString(), "4105550202", "Bo", t0),
		feedCust(uuid.NewString(), "4435550101", "Cy", t0))}
	if err := r.book.Pull(r.ctx); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]int{"0101": 2, "410555": 2, "(410) 555-0101": 1, "1-410-555-0202": 1, "41": 0, "5550101": 2, "9999": 0} {
		if got := r.phones(t, q); len(got) != want {
			t.Errorf("%q: %v, want %d", q, got, want)
		}
	}
	if _, err := r.svc.LookupCustomers(r.ctx, "410555010199"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("too long: %v", err)
	}

	// No store pin / radius: no area at all.
	l, _ := r.svc.LookupCustomers(r.ctx, "4105550101")
	if l.Area != nil || l.Customers[0].Addresses[0].Area != nil || l.Count != 3 || l.SyncedAt == nil {
		t.Fatalf("no area: %+v", l)
	}
	// A 3-mile radius around the store; one address 2 mi out, one 3.5, one unknown.
	lat, lng := 39.4, -76.6
	r.lat, r.lng, r.rad = &lat, &lng, 4828 // 3 mi
	if err := r.repo.SetCustomerAddressPoint(r.ctx, near, lat+2/69.0, lng); err != nil {
		t.Fatal(err)
	}
	if err := r.repo.SetCustomerAddressPoint(r.ctx, far, lat+3.5/69.0, lng); err != nil {
		t.Fatal(err)
	}
	l, _ = r.svc.LookupCustomers(r.ctx, "4105550101")
	if l.Area == nil || l.Area.RadiusMi != 3 || l.Area.MarginMi != 1 {
		t.Fatalf("area: %+v", l.Area)
	}
	states := map[string]string{}
	for _, a := range l.Customers[0].Addresses {
		states[a.Line1] = a.Area.State
	}
	if states["1 Test St"] != terminal.AreaIn || states["9 Far Rd"] != terminal.AreaNear || states["5 New Rd"] != terminal.AreaUnknown {
		t.Fatalf("states: %v", states)
	}
}

func TestTerminalCustomerWritesGoToTheOutbox(t *testing.T) {
	r := newCustRig(t)
	if _, err := r.svc.AddCustomer(r.ctx, till, CustomerIn{Phone: "4105550111", FirstName: "Eve"}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("no login: %v", err)
	}
	r.login(t, till, "1234")
	for _, in := range []CustomerIn{{Phone: "555-0111", FirstName: "Eve"}, {Phone: "4105550111"},
		{Phone: "4105550111", FirstName: "Eve", Address: &AddressIn{Line1: "1 Test St"}}} {
		if _, err := r.svc.AddCustomer(r.ctx, till, in); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%+v: %v", in, err)
		}
	}
	res, err := r.svc.AddCustomer(r.ctx, till, CustomerIn{Phone: "(410) 555-0111", FirstName: " Eve ", LastName: "Test", Notes: "side door",
		Address: &AddressIn{Line1: "1 Test Street", Line2: "Apt 2", City: "Towson", Zip: "21286"}})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Customer
	if !res.Created || c.Phone != "4105550111" || c.Name != "Eve Test" || !c.Pending || len(c.Addresses) != 1 || !c.Addresses[0].Default ||
		res.AddressID != c.Addresses[0].ID {
		t.Fatalf("created: %+v", res)
	}
	events, err := r.repo.ListUnsyncedEvents(r.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var up map[string]any
	types := map[string]int{}
	for _, e := range events {
		types[e.Type]++
		if e.Type == terminal.EventCustomerUpsert {
			_ = json.Unmarshal(e.Payload, &up)
		}
	}
	if types[terminal.EventCustomerUpsert] != 1 || types[terminal.EventJournalEntries] < 1 {
		t.Fatalf("outbox: %v", types)
	}
	wc := up["customer"].(map[string]any)
	if wc["id"] != c.ID || wc["phone"] != "4105550111" || wc["first_name"] != "Eve" || len(wc["addresses"].([]any)) != 1 ||
		len(up["set"].([]any)) != 2 || up["event_id"] == "" {
		t.Fatalf("customer.upsert: %v", up)
	}

	// The same phone again ("Add customer" for a new address): that customer,
	// the address added, the name kept.
	res, err = r.svc.AddCustomer(r.ctx, till, CustomerIn{Phone: "4105550111", FirstName: "Someone", Address: &AddressIn{Line1: "7 Work Rd", City: "Towson"}})
	if err != nil || res.Created || res.Customer.ID != c.ID || res.Customer.Name != "Eve Test" || len(res.Customer.Addresses) != 2 {
		t.Fatalf("same phone: %+v %v", res, err)
	}
	// The same place again: no third address, its blanks filled.
	res, err = r.svc.AddCustomerAddress(r.ctx, till, c.ID, AddressIn{Line1: "1 test st.", City: "Towson", Notes: "ring twice"})
	if err != nil || len(res.Customer.Addresses) != 2 || res.Customer.Addresses[0].Notes != "ring twice" {
		t.Fatalf("same place: %+v %v", res, err)
	}
	// Edit: names and an address by id; the phone never changes.
	first, other := "Evelyn", "4105559999"
	addr := res.Customer.Addresses[0]
	res, err = r.svc.EditCustomer(r.ctx, till, c.ID, CustomerPatch{FirstName: &first,
		Address: &AddressIn{ID: addr.ID, Line1: addr.Line1, Line2: "Apt 3", City: "Towson", Zip: "21286"}})
	if err != nil || res.Customer.Name != "Evelyn Test" || res.Customer.Addresses[0].Line2 != "Apt 3" || len(res.Customer.Addresses) != 2 {
		t.Fatalf("edit: %+v %v", res, err)
	}
	if _, err := r.svc.EditCustomer(r.ctx, till, c.ID, CustomerPatch{Phone: &other}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("phone change: %v", err)
	}
	if _, err := r.svc.EditCustomer(r.ctx, till, uuid.NewString(), CustomerPatch{FirstName: &first}); !errors.Is(err, terminal.ErrCustomerNotFound) {
		t.Fatalf("404: %v", err)
	}
	if got := r.phones(t, "0111"); len(got) != 1 {
		t.Fatalf("lookup the new one: %v", got)
	}

	// main's older copy (written before the terminal's change) doesn't
	// overwrite it; main's copy after it does (and clears pending).
	r.feed.pages = []any{page("c1", false, nil, feedCust(c.ID, "4105550111", "Old", time.Now().UTC().Add(-time.Hour)))}
	if err := r.book.Pull(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.repo.Customer(r.ctx, c.ID); got.FirstName != "Evelyn" || !got.Pending {
		t.Fatalf("older feed copy: %+v", got)
	}
	r.feed.pages = []any{page("c2", false, nil, feedCust(c.ID, "4105550111", "Evelyn", time.Now().UTC().Add(time.Minute),
		map[string]any{"id": addr.ID, "line1": addr.Line1, "line2": "Apt 3", "city": "Towson", "zip": "21286", "default": true}))}
	if err := r.book.Pull(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.repo.Customer(r.ctx, c.ID); got.Pending || len(got.Addresses) != 1 {
		t.Fatalf("main's newer copy: %+v", got)
	}

	// The Pi's own copy main merged into another id (same phone): replaced.
	merged := uuid.NewString()
	r.feed.pages = []any{page("c3", false, nil, feedCust(merged, "4105550111", "Evelyn", time.Now().UTC().Add(2*time.Minute)))}
	if err := r.book.Pull(r.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.repo.Customer(r.ctx, c.ID); !errors.Is(err, terminal.ErrCustomerNotFound) {
		t.Fatalf("old id kept: %v", err)
	}
	if got := r.phones(t, "4105550111"); len(got) != 1 {
		t.Fatalf("merged: %v", got)
	}
}

func TestCustomerOrdersAndLastOrder(t *testing.T) {
	r := newCustRig(t)
	r.login(t, till, "1234")
	res, err := r.svc.AddCustomer(r.ctx, till, CustomerIn{Phone: "4105550121", FirstName: "Flo", LastName: "Test"})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Customer.ID
	placed, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-cust-0001", Type: terminal.TypePickup,
		Customer: &Customer{ID: id, Name: "Flo", Phone: "4105550121"},
		Cart:     Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: 1, Selection: pricing.Selection{ItemID: soda}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-cust-0002", Type: terminal.TypePickup,
		Customer: &Customer{ID: uuid.NewString()}, Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: 1, Selection: pricing.Selection{ItemID: soda}}}}}}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("unknown customer id: %v", err)
	}
	// An online order for the same phone, written another way.
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "x-1", PlacedAt: time.Now().UTC().Add(time.Minute), Fulfillment: order.Fulfillment{Type: "pickup"},
		Customer: order.Customer{DisplayName: "Flo", Phone: "+1 (410) 555-0121"}, Money: order.Money{Subtotal: 900, Total: 900}}
	if _, err := r.repo.UpsertArrived(r.ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := r.svc.CustomerOrdersOf(r.ctx, id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Orders) != 2 || got.Orders[0].Order.ID != s.ID || got.Orders[1].Order.ID != placed.Order.Order.ID || got.Source != "pi" {
		t.Fatalf("orders: %+v", got)
	}
	if customerIDOf(placed.Order.Order) != id {
		t.Fatalf("the order carries the customer id: %v", placed.Order.Order.SourceMeta)
	}
	if c, _ := r.repo.Customer(r.ctx, id); c.LastOrderAt == "" {
		t.Fatalf("last order not noted: %+v", c)
	}
}
