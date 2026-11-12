package terminal

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/rs/zerolog"
)

// The store-side security round (2026-11-12): the PIN guard (H3), one
// approval per need (M2), the session's idle expiry (M4), the device-level
// manager check (M7). The rung rule (M3) is in rung_test.go.

// H3: 200 wrong PINs at once on one terminal are 5 checks at most (the slot
// is reserved before bcrypt), and 200 at once from 40 terminals stop at the
// store's 10.
func TestParallelWrongPINsStopAtTheLimit(t *testing.T) {
	r := newRig(t)
	r.svc.SetPinRepo(r.ctx, r.repo)
	run := func(n int, actor func(i int) Actor) (wrong, throttled int64) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, err := r.svc.Login(r.ctx, actor(i), fmt.Sprintf("%04d", 7000+i))
				var thr *terminal.Throttled
				switch {
				case errors.Is(err, terminal.ErrWrongPIN):
					atomic.AddInt64(&wrong, 1)
				case errors.As(err, &thr):
					atomic.AddInt64(&throttled, 1)
				default:
					t.Errorf("login %d: %v", i, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		return wrong, throttled
	}
	wrong, throttled := run(200, func(int) Actor { return till })
	if wrong > pinFreeMisses || wrong+throttled != 200 {
		t.Fatalf("one terminal: %d PINs were checked (limit %d), %d refused", wrong, pinFreeMisses, throttled)
	}
	more, _ := run(200, func(i int) Actor { return Actor{DeviceID: fmt.Sprintf("dev-x%d", i%40), DeviceName: "Till x"} })
	if wrong+more > pinStoreMisses {
		t.Fatalf("the store: %d PINs were checked (limit %d)", wrong+more, pinStoreMisses)
	}
	if g := r.svc.PinGuard(); g.Misses24h != int(wrong+more) {
		t.Fatalf("every checked PIN is counted: %+v vs %d", g, wrong+more)
	}
}

// H3: a slow guesser never trips the 15-minute throttle; the daily ceiling
// locks every PIN check, pages the owner once, survives a restart, and a
// manager unlocks it. The ceiling is a setting.
func TestDailyCeilingLocksUntilAManagerUnlocks(t *testing.T) {
	r := newRig(t)
	pg := &pages{}
	r.svc.SetPager(pg)
	r.svc.SetPinRepo(r.ctx, r.repo)
	clock := time.Now()
	r.svc.now = func() time.Time { return clock }
	if _, err := r.svc.SetPinCeiling(r.ctx, 5); err == nil {
		t.Fatal("a ceiling under 10 was taken")
	}
	if g, err := r.svc.SetPinCeiling(r.ctx, 12); err != nil || g.DailyCeiling != 12 {
		t.Fatalf("ceiling: %+v %v", g, err)
	}
	r.login(t, till, "1234")
	// One wrong PIN every 20 minutes: never throttled, locked at the 12th.
	for i := 0; i < 12; i++ {
		for k := 0; k < 5; k++ { // 20 minutes; the terminal keeps polling the Pi
			clock = clock.Add(4 * time.Minute)
			r.svc.Touch(till)
		}
		if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "0000"}); !errors.Is(err, terminal.ErrWrongPIN) {
			t.Fatalf("miss %d: %v", i, err)
		}
	}
	var locked *terminal.PinLocked
	if _, err := r.svc.Login(r.ctx, Actor{DeviceID: "dev-2", DeviceName: "Till 2"}, "1234"); !errors.As(err, &locked) {
		t.Fatalf("a right PIN on another terminal after the ceiling: %v", err)
	}
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876"}); !errors.As(err, &locked) {
		t.Fatalf("manager-check after the ceiling: %v", err)
	}
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(pizzaLine(), 1000)}}}
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Cart: cart, ManagerPIN: "9876"}); !errors.As(err, &locked) {
		t.Fatalf("an inline manager PIN after the ceiling: %v", err)
	}
	// The cashier already logged in goes on working.
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}}); err != nil {
		t.Fatalf("a logged-in cashier while locked: %v", err)
	}
	if g := r.svc.PinGuard(); !g.Locked || g.LockedAt == nil || g.Misses24h != 12 || g.DailyCeiling != 12 {
		t.Fatalf("guard: %+v", g)
	}
	if j := journalOf(t, r, terminal.ActionPinLocked); len(j) != 1 {
		t.Fatalf("pin.locked journal: %v", j)
	}
	if r.pub.count(EventPins) != 1 {
		t.Fatalf("terminal.pins events: %d", r.pub.count(EventPins))
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(pg.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m := pg.all(); len(m) != 1 {
		t.Fatalf("the owner is paged once: %v", m)
	}
	// A restart keeps the lock.
	svc2 := NewService(r.svc.Store(), r.repo, r.orders, r.pub, zerolog.Nop())
	svc2.now = r.svc.now
	svc2.SetPinRepo(r.ctx, r.repo)
	if _, err := svc2.Login(r.ctx, till, "1234"); !errors.As(err, &locked) {
		t.Fatalf("a restart cleared the lock: %v", err)
	}
	// A manager unlocks: PINs work again at once, the count starts over.
	if g := r.svc.UnlockPins(r.ctx, "Pi web page"); g.Locked || g.Misses24h != 0 {
		t.Fatalf("unlocked: %+v", g)
	}
	if _, err := r.svc.Login(r.ctx, Actor{DeviceID: "dev-2", DeviceName: "Till 2"}, "1234"); err != nil {
		t.Fatalf("login after the unlock: %v", err)
	}
	if j := journalOf(t, r, terminal.ActionPinUnlocked); len(j) != 1 || j[0]["by"] != "Pi web page" {
		t.Fatalf("pin.unlocked journal: %v", j)
	}
}

// A manager's PIN is exactly 4 digits: a shorter one that is a manager's is
// a wrong PIN (counted), a cashier's 2-digit PIN still works.
func TestShortManagerPINIsRefused(t *testing.T) {
	r := newRig(t)
	r.main.set(PathTerminalConfig, staffConfig(t, 2, terminal.Staff{ID: "mgr-short", FirstName: "Sam", Role: terminal.RoleManager, PinHash: hash(t, "321")}))
	if changed, err := r.svc.Store().Refresh(r.ctx, terminal.KindTerminalConfig); err != nil || !changed {
		t.Fatalf("config: %v %v", changed, err)
	}
	if _, err := r.svc.Login(r.ctx, till, "321"); !errors.Is(err, terminal.ErrManagerPINShort) || !errors.Is(err, terminal.ErrWrongPIN) {
		t.Fatalf("a 3-digit manager PIN logged in: %v", err)
	}
	r.login(t, till, "55") // a cashier's 2-digit PIN
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "321"}); !errors.Is(err, terminal.ErrManagerPINShort) {
		t.Fatalf("manager-check with a 3-digit manager PIN: %v", err)
	}
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(pizzaLine(), 500)}}}
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Cart: cart, ManagerPIN: "321"}); !errors.Is(err, terminal.ErrManagerPINShortInline) ||
		!errors.Is(err, terminal.ErrManagerPINWrong) {
		t.Fatalf("an inline 3-digit manager PIN: %v", err)
	}
	if g := r.svc.PinGuard(); g.Misses24h != 3 {
		t.Fatalf("each one counts as a wrong PIN: %+v", g)
	}
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876"}); err != nil {
		t.Fatalf("the 4-digit manager: %v", err)
	}
}

// M4: a session with no call from its terminal for the auto-lock time + a
// margin is over on the Pi; a terminal that keeps talking keeps it.
func TestSessionIdlesOutOnThePi(t *testing.T) {
	r := newRig(t) // auto_lock_minutes 3 -> 5 minutes
	clock := time.Now()
	r.svc.now = func() time.Time { return clock }
	r.login(t, till, "1234")
	for i := 0; i < 10; i++ { // 40 minutes of status polls
		clock = clock.Add(4 * time.Minute)
		r.svc.Touch(till)
	}
	if v := r.svc.Session(till); v.Staff == nil {
		t.Fatal("a terminal that keeps calling lost its session")
	}
	clock = clock.Add(5*time.Minute + time.Second)
	r.svc.Touch(till) // the first call after the silence doesn't bring it back
	if v := r.svc.Session(till); v.Staff != nil {
		t.Fatalf("the session outlived the silence: %+v", v.Staff)
	}
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("an order on an idled-out session: %v", err)
	}
	j := journalOf(t, r, terminal.ActionSessionEnded)
	if len(j) != 1 || j[0]["reason"] != terminal.SessionIdle || j[0]["_staff"] != "cash-1" {
		t.Fatalf("session.ended journal: %v", j)
	}
	r.login(t, till, "1234")
	if v := r.svc.Session(till); v.Staff == nil {
		t.Fatal("login after the idle-out")
	}
}

// M2: each manager need takes its own approval. One approval for the
// discount used to cover the price drop too.
func TestOneApprovalPerNeed(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234") // discount limit 10 %, price drop 20 %
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(pizzaLine(), 1000)}}, Discount: &Discount{Pct: 30, Reason: "regular"}}
	disc, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: terminal.ActionDiscount})
	if err != nil {
		t.Fatal(err)
	}
	var mr *terminal.ManagerRequired
	in := OrderIn{ClientKey: tk(), Cart: cart, Approvals: []string{disc.ApprovalID}}
	if _, err := r.svc.PlaceOrder(r.ctx, till, in); !errors.As(err, &mr) || mr.Action != terminal.ActionPriceChange {
		t.Fatalf("the discount's approval covered the price drop: %v", err)
	}
	// Nothing was used up: the same approval + one for the price drop pass.
	price, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: terminal.ActionPriceChange})
	if err != nil {
		t.Fatal(err)
	}
	in.Approvals = []string{disc.ApprovalID, price.ApprovalID}
	res, err := r.svc.PlaceOrder(r.ctx, till, in)
	if err != nil {
		t.Fatalf("one approval per need: %v", err)
	}
	if j := journalOf(t, r, terminal.ActionOverride); len(j) != 2 {
		t.Fatalf("an override row per need: %v", j)
	}
	// Both are used up now.
	again := OrderIn{ClientKey: tk(), Cart: cart, Approvals: in.Approvals}
	if _, err := r.svc.PlaceOrder(r.ctx, till, again); !errors.As(err, &mr) {
		t.Fatalf("approvals are single use: %v", err)
	}
	// Two approvals for the same action don't cover another one.
	d2, _ := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: terminal.ActionDiscount})
	d3, _ := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: terminal.ActionDiscount})
	again.Approvals = []string{d2.ApprovalID, d3.ApprovalID}
	if _, err := r.svc.PlaceOrder(r.ctx, till, again); !errors.As(err, &mr) || mr.Action != terminal.ActionPriceChange {
		t.Fatalf("two discount approvals covered the price drop: %v", err)
	}
	// A manager's PIN with the request still covers every need.
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Cart: cart, ManagerPIN: "9876"}); err != nil {
		t.Fatalf("one PIN for the whole request: %v", err)
	}
	_ = res
}

// M7: admin mode's drawer test, Unpair and the pairing window ask a
// manager's PIN with nobody logged in; anything else still needs a login.
func TestManagerCheckWithNobodyLoggedIn(t *testing.T) {
	r := newRig(t)
	for _, action := range []string{"admin_drawer_test", "unpair", terminal.NeedPairingWindow} {
		res, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: action})
		if err != nil || !res.OK || res.Staff.ID != "mgr-1" {
			t.Fatalf("%s with nobody logged in: %+v %v", action, res, err)
		}
	}
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "1234", Action: "unpair"}); !errors.Is(err, terminal.ErrWrongPIN) {
		t.Fatalf("a cashier's PIN: %v", err)
	}
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: terminal.ActionRefund}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("a money action with nobody logged in: %v", err)
	}
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876"}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("the default action with nobody logged in: %v", err)
	}
}

// The owner, 2026-11-12 evening: taking a sent line off (the check gets
// smaller) is a void of what the kitchen got: a manager's. Adding, or a
// change that leaves the check as big or bigger, stays free; a manager does
// it alone; the approval is for "void".
func TestSentLineRemovalNeedsAManager(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	soda := func(n int) pricing.Line { return pricing.Line{Qty: n, Selection: pricing.Selection{ItemID: soda}} }
	cart := func(lines ...pricing.Line) Cart { return Cart{Cart: pricing.Cart{Lines: lines}} }
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Cart: cart(pizzaLine(), soda(2))})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	// One more soda: free.
	ch, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart(pizzaLine(), soda(3))})
	if err != nil || len(ch.Added) != 1 {
		t.Fatalf("adding to a sent order: %+v %v", ch, err)
	}
	more := ch.Order.Order.Money.Total
	// The sodas swapped for a second pizza (a line goes, the check grows): free.
	if ch, err = r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart(pizzaLine(), pizzaLine())}); err != nil || len(ch.Removed) == 0 ||
		ch.Order.Order.Money.Total <= more {
		t.Fatalf("a swap for more: %+v %v", ch, err)
	}
	big := ch.Order.Order.Money.Total
	// A pizza off: the check gets smaller -> a manager, requested = the drop.
	var mr *terminal.ManagerRequired
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart(pizzaLine())}); !errors.As(err, &mr) ||
		mr.Action != terminal.ActionVoid || mr.Requested != big-mustTotal(t, r, cart(pizzaLine())) {
		t.Fatalf("a sent line off without a manager: %v %+v", err, mr)
	}
	// An approval for another action doesn't cover it; one for "void" does.
	wrong, _ := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: terminal.ActionDiscount})
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart(pizzaLine()), Approvals: []string{wrong.ApprovalID}}); !errors.As(err, &mr) {
		t.Fatalf("a discount approval covered a void: %v", err)
	}
	ok, _ := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: terminal.ActionVoid})
	ch, err = r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart(pizzaLine()), Approvals: []string{ok.ApprovalID}})
	if err != nil || len(ch.Removed) != 1 {
		t.Fatalf("with the void approval: %+v %v", ch, err)
	}
	if j := journalOf(t, r, terminal.ActionOverride); len(j) != 1 || j[0]["for"] != terminal.ActionVoid || j[0]["manager_id"] != "mgr-1" {
		t.Fatalf("the override: %v", j)
	}
	// A manager takes a line off alone.
	mgrTill := Actor{DeviceID: "dev-2", DeviceName: "Till 2"}
	r.login(t, mgrTill, "9876")
	res2, err := r.svc.PlaceOrder(r.ctx, mgrTill, OrderIn{ClientKey: tk(), Cart: cart(pizzaLine(), soda(2))})
	if err != nil {
		t.Fatal(err)
	}
	if ch, err := r.svc.ChangeCart(r.ctx, mgrTill, res2.Order.Order.ID, ChangeIn{ClientKey: tk(), Cart: cart(pizzaLine())}); err != nil || len(ch.Removed) != 1 {
		t.Fatalf("a manager takes a sent line off: %+v %v", ch, err)
	}
}

func mustTotal(t *testing.T, r *rig, c Cart) int64 {
	t.Helper()
	v, err := r.svc.Quote(r.ctx, till, QuoteIn{Type: "counter", Cart: c})
	if err != nil {
		t.Fatal(err)
	}
	return v.Quote.(pricing.Quote).TotalCents
}
