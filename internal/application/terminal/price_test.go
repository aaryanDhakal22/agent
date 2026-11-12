package terminal

import (
	"encoding/json"
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// Item price changes (TERMINAL-PLAN "Item price change"): raising is free, a
// cashier lowers by up to cashier_price_drop_pct (default 20) alone, more
// needs a manager's PIN; every change is journaled.

func atPrice(l pricing.Line, cents int64) pricing.Line {
	l.PriceCents = &cents
	return l
}

func journalOf(t *testing.T, r *rig, action string) []map[string]any {
	t.Helper()
	all, err := r.repo.Journal(r.ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range all {
		if e.Action == action {
			var d map[string]any
			_ = json.Unmarshal(e.Detail, &d)
			d["_staff"] = e.StaffID
			out = append(out, d)
		}
	}
	return out
}

func TestPriceChangeLimits(t *testing.T) {
	r := newRig(t)
	soda1 := pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: soda}}
	// Not logged in: a raise needs nobody, a drop would need a manager.
	q, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(soda1, 250)}}}})
	if err != nil || q.NeedsManager || len(q.PriceChanges) != 1 || q.PriceChanges[0].FromCents != 199 || q.PriceChanges[0].ToCents != 250 {
		t.Fatalf("raise: %+v %v", q, err)
	}
	r.login(t, till, "1234") // cashier: 20 % (the default)
	// The pizza (Cheese 16 XL with Hawaiian on a half = 1999) at 1700: 15 % off, alone.
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(pizzaLine(), 1700), atPrice(soda1, 250)}}}
	q, err = r.svc.Quote(r.ctx, till, QuoteIn{Cart: cart})
	quote := q.Quote.(pricing.Quote)
	if err != nil || q.NeedsManager || quote.Lines[0].UnitCents != 1700+225 || *quote.Lines[0].ListCents != 1999 || q.PriceChanges[0].DropPct != 15 {
		t.Fatalf("within the limit: %+v %+v %v", q, quote.Lines, err)
	}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-price-0001", Cart: cart})
	if err != nil {
		t.Fatal(err)
	}
	if it := res.Order.Order.Items[0]; it.UnitPrice != 1700 || it.LineTotal != 1925 || res.Order.Order.Money.Total != quote.TotalCents {
		t.Fatalf("order line: %+v money %+v", it, res.Order.Order.Money)
	}
	if j := journalOf(t, r, terminal.ActionPriceChange); len(j) != 2 || j[0]["approved_by"] != nil || j[0]["_staff"] != "cash-1" {
		t.Fatalf("journal: %+v", j)
	}
	// 1500 = 25 % off: a manager's PIN.
	cart = Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(pizzaLine(), 1500)}}}
	if q, _ := r.svc.Quote(r.ctx, till, QuoteIn{Cart: cart}); !q.NeedsManager || !q.PriceChanges[0].NeedsManager {
		t.Fatalf("quote over the limit: %+v", q)
	}
	in := OrderIn{ClientKey: "k-price-0002", Cart: cart}
	var mr *terminal.ManagerRequired
	if _, err := r.svc.PlaceOrder(r.ctx, till, in); !errors.As(err, &mr) || mr.Action != terminal.ActionPriceChange || mr.Limit != 20 || mr.Requested != 25 {
		t.Fatalf("manager required: %v", err)
	}
	in.ManagerPIN = "55" // a cashier's PIN
	if _, err := r.svc.PlaceOrder(r.ctx, till, in); !errors.Is(err, terminal.ErrManagerPINWrong) {
		t.Fatalf("not a manager: %v", err)
	}
	in.ManagerPIN = "9876"
	res, err = r.svc.PlaceOrder(r.ctx, till, in)
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	var approved, override bool
	for _, d := range journalOf(t, r, terminal.ActionPriceChange) {
		approved = approved || (d["approved_by"] == "mgr-1" && d["from_cents"] == 1999.0 && d["to_cents"] == 1500.0)
	}
	for _, d := range journalOf(t, r, terminal.ActionOverride) {
		override = override || (d["for"] == terminal.ActionPriceChange && d["manager_id"] == "mgr-1")
	}
	if !approved || !override {
		t.Fatal("the approved change and the override are journaled")
	}
	// A kitchen change keeps the approved price without asking again ...
	before := len(journalOf(t, r, terminal.ActionPriceChange))
	ch, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(pizzaLine(), 1500), soda1}}}})
	if err != nil || !ch.Changed {
		t.Fatalf("change keeps the price: %+v %v", ch, err)
	}
	if len(journalOf(t, r, terminal.ActionPriceChange)) != before {
		t.Fatal("an unchanged price change isn't journaled again")
	}
	// ... but a new drop over the limit needs one.
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(pizzaLine(), 1500), atPrice(soda1, 100)}}}}); !errors.As(err, &mr) || mr.Requested != 50 {
		t.Fatalf("a new drop in a change: %v", err)
	}
	// A manager is unlimited; $0 is a price.
	r.login(t, till, "9876")
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-price-0003", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(pizzaLine(), 0)}}}}); err != nil {
		t.Fatalf("a manager: %v", err)
	}
	// The store's setting: 0 % = every drop needs a manager.
	cfg := staffConfig(t, 2)
	cfg["settings"].(map[string]any)["cashier_price_drop_pct"] = 0
	r.main.set(PathTerminalConfig, cfg)
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	r.login(t, till, "1234")
	if q, _ := r.svc.Quote(r.ctx, till, QuoteIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(soda1, 198)}}}}); !q.NeedsManager {
		t.Fatalf("0 %%: %+v", q)
	}
	// A combo's items keep their prices; a price out of range is refused.
	var ci *terminal.CartInvalid
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-price-0004", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(soda1, -1)}}}}); !errors.As(err, &ci) || ci.Issues[0].Code != pricing.IssueOpenPrice {
		t.Fatalf("out of range: %v", err)
	}
}

func TestManagerCheck(t *testing.T) {
	r := newRig(t)
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876"}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("no login: %v", err)
	}
	r.login(t, till, "1234")
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9x"}); !errors.Is(err, terminal.ErrBadPIN) {
		t.Fatalf("bad pin: %v", err)
	}
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "55"}); !errors.Is(err, terminal.ErrWrongPIN) {
		t.Fatalf("a cashier's PIN: %v", err)
	}
	res, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: "price_change"})
	if err != nil || !res.OK || res.Staff.ID != "mgr-1" || res.Staff.Role != terminal.RoleManager || res.Staff.Name != "Khawar A." {
		t.Fatalf("manager: %+v %v", res, err)
	}
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: "Price Change!"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad action: %v", err)
	}
	j := journalOf(t, r, terminal.ActionManagerCheck)
	if len(j) != 2 || j[0]["ok"] != true || j[0]["manager_id"] != "mgr-1" || j[1]["ok"] != false || j[0]["_staff"] != "cash-1" {
		t.Fatalf("journal: %+v", j)
	}
	// Throttled with the logins: five misses, then a wait (also for the right PIN).
	for i := 0; i < pinFreeMisses; i++ {
		if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "0000"}); !errors.Is(err, terminal.ErrWrongPIN) {
			t.Fatalf("miss %d: %v", i, err)
		}
	}
	var thr *terminal.Throttled
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876"}); !errors.As(err, &thr) {
		t.Fatalf("throttled: %v", err)
	}
	if err := terminal.CheckPostedAction(terminal.ActionManagerCheck); err == nil {
		t.Fatal("manager_check is the Pi's own journal action")
	}
}
