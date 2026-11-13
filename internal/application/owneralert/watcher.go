package owneralert

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/safego"
)

// PrinterState is one printer as the probes see it (printer.Registry).
type PrinterState struct {
	Name string
	// Down: failing without a break since DownSince.
	Down      bool
	DownSince time.Time
}

// Link is the Pi's connection to main.
type Link struct {
	Linked    bool // a store key: an unlinked Pi has nothing to lose
	Connected bool
	// Refused: main refused the key (a revoke / re-link — the developer's
	// problem, not the owner's).
	Refused bool
}

// Batch is the open batch.
type Batch struct {
	OpenedAt time.Time
	Orders   int64
}

// Inputs are the watcher's read-only views (wired in cmd/main.go).
type Inputs struct {
	Store    func() (storeconfig.StoreContext, bool)
	Printers func() []PrinterState
	Link     func() Link
	// Waiting: when each order still waiting to be accepted arrived.
	Waiting func(ctx context.Context) ([]time.Time, error)
	// OpenBatch: the open batch (ok=false: none).
	OpenBatch func(ctx context.Context) (Batch, bool, error)
}

// Tick is how often the watcher looks.
const Tick = 20 * time.Second

// staleBatch: at the morning check, an open batch with orders opened more
// than this long before is yesterday's (a batch closed last night opened
// after ~18:00 the day before at the latest; 16 h before a 10:00 check).
const staleBatch = 16 * time.Hour

// notPrintedTTL: an unprinted-order alert Pushover didn't take is tried
// again for this long.
const notPrintedTTL = 30 * time.Minute

type printerInc struct {
	told bool // the down message was sent
}

type pendingOrder struct {
	id, label string
	at        time.Time
}

// Watcher turns the floor's state into owner alerts, through the Policy.
type Watcher struct {
	in     Inputs
	pol    *Policy
	logger zerolog.Logger

	mu       sync.Mutex
	printers map[string]*printerInc
	// offline: since when main is unreachable (zero = connected); done =
	// the outage is dealt with, told = its message was sent.
	offlineSince time.Time
	offlineDone  bool
	offlineTold  bool
	// waitingDone: the current waiting incident is dealt with.
	waitingDone bool
	notPrinted  []pendingOrder
}

// New builds the watcher.
func New(in Inputs, pol *Policy, logger zerolog.Logger) *Watcher {
	return &Watcher{in: in, pol: pol, logger: logger.With().Str("module", "owneralert").Logger(),
		printers: map[string]*printerInc{}}
}

// Run checks every Tick until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			func() {
				defer safego.Recover("owneralert.tick")
				w.Check(ctx, now)
			}()
		}
	}
}

// NotPrinted queues an order that printed on no printer (the order
// service's print path; never blocks).
func (w *Watcher) NotPrinted(orderID, label string, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, p := range w.notPrinted {
		if p.id == orderID {
			return
		}
	}
	w.notPrinted = append(w.notPrinted, pendingOrder{id: orderID, label: label, at: at})
}

func (w *Watcher) env(now time.Time) (Env, *storeconfig.StoreContext) {
	var sc *storeconfig.StoreContext
	if w.in.Store != nil {
		if s, ok := w.in.Store(); ok {
			sc = &s
		}
	}
	e := Env{Now: now, Open: OpenAt(sc, now), Settings: SettingsFrom(sc)}
	if sc != nil {
		e.Title = sc.StoreName()
		e.OfflineByMain = sc.OwnerOfflineAlertByMain
	}
	return e, sc
}

// Check runs every rule once at `now` (the ticker; tests call it).
func (w *Watcher) Check(ctx context.Context, now time.Time) {
	env, sc := w.env(now)
	w.checkPrinters(ctx, env)
	w.checkNotPrinted(ctx, env)
	w.checkLink(ctx, env)
	w.checkWaiting(ctx, env)
	w.checkClose(ctx, env, sc)
	w.pol.Flush(ctx, env)
}

func (w *Watcher) checkPrinters(ctx context.Context, env Env) {
	if w.in.Printers == nil {
		return
	}
	seen := map[string]bool{}
	for _, p := range w.in.Printers() {
		seen[p.Name] = true
		w.mu.Lock()
		inc := w.printers[p.Name]
		w.mu.Unlock()
		down := p.Down && !p.DownSince.IsZero() && env.Now.Sub(p.DownSince) >= env.Settings.PrinterDown
		switch {
		case down && inc == nil:
			out := w.pol.Offer(ctx, env, PrinterDown(p.Name))
			if out.Done() {
				w.mu.Lock()
				w.printers[p.Name] = &printerInc{told: out == Sent}
				w.mu.Unlock()
			}
		case !p.Down && inc != nil:
			w.printerBack(ctx, env, p.Name, inc)
		}
	}
	// A printer gone from the roster: its outage is over.
	w.mu.Lock()
	var gone []string
	for name := range w.printers {
		if !seen[name] {
			gone = append(gone, name)
		}
	}
	for _, name := range gone {
		delete(w.printers, name)
	}
	w.mu.Unlock()
}

func (w *Watcher) printerBack(ctx context.Context, env Env, name string, inc *printerInc) {
	if inc.told {
		if out := w.pol.Offer(ctx, env, PrinterBack(name)); out == Failed {
			return // tried again next tick
		}
	}
	w.mu.Lock()
	delete(w.printers, name)
	w.mu.Unlock()
}

func (w *Watcher) checkNotPrinted(ctx context.Context, env Env) {
	w.mu.Lock()
	list := w.notPrinted
	w.notPrinted = nil
	w.mu.Unlock()
	var keep []pendingOrder
	for _, p := range list {
		out := w.pol.Offer(ctx, env, NotPrinted(p.id, p.label))
		if out == Failed && env.Now.Sub(p.at) < notPrintedTTL {
			keep = append(keep, p)
		}
	}
	if len(keep) > 0 {
		w.mu.Lock()
		w.notPrinted = append(keep, w.notPrinted...)
		w.mu.Unlock()
	}
}

func (w *Watcher) checkLink(ctx context.Context, env Env) {
	if w.in.Link == nil {
		return
	}
	l := w.in.Link()
	w.mu.Lock()
	since, told, done := w.offlineSince, w.offlineTold, w.offlineDone
	w.mu.Unlock()
	if !l.Linked || l.Refused || l.Connected {
		if told && l.Connected {
			if out := w.pol.Offer(ctx, env, Online()); out == Failed {
				return // tried again next tick
			}
		}
		w.mu.Lock()
		w.offlineSince, w.offlineTold, w.offlineDone = time.Time{}, false, false
		w.mu.Unlock()
		return
	}
	if env.OfflineByMain && !told {
		// main pages the owner for this one (main 4.0.2, contract §2.1e): it
		// can when the store's internet or the Pi is down, the Pi can't. One
		// alert, never two. (An outage this Pi already told still gets its
		// "back online" from here.)
		w.mu.Lock()
		w.offlineSince, w.offlineDone = time.Time{}, false
		w.mu.Unlock()
		return
	}
	if since.IsZero() {
		since = env.Now
		w.mu.Lock()
		w.offlineSince = since
		w.mu.Unlock()
	}
	if done || env.Now.Sub(since) < env.Settings.Offline {
		return
	}
	out := w.pol.Offer(ctx, env, Offline(env.Now.Sub(since)))
	if out.Done() {
		w.mu.Lock()
		w.offlineDone = true
		w.offlineTold = out == Sent // held / off: no "back online" either
		w.mu.Unlock()
	}
}

// waitingNow: an incident = WaitingOrders orders each over WaitingFor, or
// one over OrderWait. Returns the count over WaitingFor and the oldest wait.
func waitingNow(arrivals []time.Time, now time.Time, s Settings) (bool, int, time.Duration) {
	n := 0
	var oldest time.Duration
	for _, a := range arrivals {
		d := now.Sub(a)
		if d >= s.WaitingFor {
			n++
		}
		if d > oldest {
			oldest = d
		}
	}
	return n >= s.WaitingOrders || oldest >= s.OrderWait, n, oldest
}

func (w *Watcher) checkWaiting(ctx context.Context, env Env) {
	if w.in.Waiting == nil {
		return
	}
	arrivals, err := w.in.Waiting(ctx)
	if err != nil {
		return
	}
	hit, n, oldest := waitingNow(arrivals, env.Now, env.Settings)
	w.mu.Lock()
	if !hit {
		w.waitingDone = false // the incident is over
	}
	done := w.waitingDone
	w.mu.Unlock()
	if !hit || done {
		return
	}
	if n == 0 {
		n = 1
	}
	if out := w.pol.Offer(ctx, env, Waiting(n, oldest)); out.Done() {
		w.mu.Lock()
		w.waitingDone = true
		w.mu.Unlock()
	}
}

func (w *Watcher) checkClose(ctx context.Context, env Env, sc *storeconfig.StoreContext) {
	if w.in.OpenBatch == nil {
		return
	}
	local := env.Now.In(Location(sc))
	day := local.Format("2006-01-02")
	if local.Hour() < env.Settings.CloseCheckHour || w.pol.CloseDay() == day {
		return
	}
	b, ok, err := w.in.OpenBatch(ctx)
	if err != nil {
		return
	}
	check := time.Date(local.Year(), local.Month(), local.Day(), env.Settings.CloseCheckHour, 0, 0, 0, local.Location())
	if !ok || b.Orders == 0 || check.Sub(b.OpenedAt) < staleBatch {
		w.pol.SetCloseDay(day) // closed last night (or nothing to close)
		return
	}
	if out := w.pol.Offer(ctx, env, CloseBatch(day)); out.Done() || out == Cooldown {
		w.pol.SetCloseDay(day)
	}
}
