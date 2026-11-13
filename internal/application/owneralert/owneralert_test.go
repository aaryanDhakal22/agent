package owneralert

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"quiccpos/agent/internal/domain/storeconfig"
)

type sink struct {
	mu   sync.Mutex
	msgs []string
	fail bool
}

func (s *sink) send(_ context.Context, _, text, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("pushover down")
	}
	s.msgs = append(s.msgs, text)
	return nil
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

func (s *sink) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) == 0 {
		return ""
	}
	return s.msgs[len(s.msgs)-1]
}

// rig: a store open 11:00-23:00 in New York, a fake clock and inputs.
type rig struct {
	t        *testing.T
	now      time.Time
	sink     *sink
	pol      *Policy
	w        *Watcher
	printers []PrinterState
	link     Link
	waiting  []time.Time
	batch    *Batch
	cfg      map[string]any
	byMain   bool // store_config owner_offline_alert_by_main
}

var ny, _ = time.LoadLocation("America/New_York")

func at(day, hh, mm int) time.Time { return time.Date(2026, 11, day, hh, mm, 0, 0, ny) }

func newRig(t *testing.T, dir string) *rig {
	r := &rig{t: t, now: at(12, 12, 0), sink: &sink{}, link: Link{Linked: true, Connected: true},
		cfg: map[string]any{"opening_hours": map[string]any{
			"mon": []any{map[string]any{"open": "11:00", "close": "23:00"}},
			"tue": []any{map[string]any{"open": "11:00", "close": "23:00"}},
			"wed": []any{map[string]any{"open": "11:00", "close": "23:00"}},
			"thu": []any{map[string]any{"open": "11:00", "close": "23:00"}},
			"fri": []any{map[string]any{"open": "11:00", "close": "23:00"}},
			"sat": []any{map[string]any{"open": "11:00", "close": "23:00"}},
			"sun": []any{map[string]any{"open": "11:00", "close": "23:00"}},
		}}}
	path := ""
	if dir != "" {
		path = StatePath(dir + "/agent.db")
	}
	r.pol = NewPolicy(path, r.sink.send, zerolog.Nop())
	r.w = New(Inputs{
		Store: func() (storeconfig.StoreContext, bool) {
			var sc storeconfig.StoreContext
			sc.Location.Name = "Lombardi's"
			sc.Location.Timezone = "America/New_York"
			sc.Location.Config = r.cfg
			sc.OwnerOfflineAlertByMain = r.byMain
			return sc, true
		},
		Printers: func() []PrinterState { return r.printers },
		Link:     func() Link { return r.link },
		Waiting:  func(context.Context) ([]time.Time, error) { return r.waiting, nil },
		OpenBatch: func(context.Context) (Batch, bool, error) {
			if r.batch == nil {
				return Batch{}, false, nil
			}
			return *r.batch, true, nil
		},
	}, r.pol, zerolog.Nop())
	return r
}

// run advances the clock in watcher ticks.
func (r *rig) run(d time.Duration) {
	end := r.now.Add(d)
	for !r.now.After(end) {
		r.w.Check(context.Background(), r.now)
		r.now = r.now.Add(Tick)
	}
}

func TestPrinterDownNeedsFiveMinutesAndTellsOnce(t *testing.T) {
	r := newRig(t, "")
	r.printers = []PrinterState{{Name: "Pizza", Down: true, DownSince: r.now}}
	r.run(4 * time.Minute)
	if r.sink.count() != 0 {
		t.Fatalf("sent before 5 min: %v", r.sink.msgs)
	}
	r.run(30 * time.Minute) // still down: one message, not a stream
	if r.sink.count() != 1 || r.sink.last() != "The Pizza printer is off or out of paper. Check it: turn it on, add paper, and check its cable." {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
	r.printers[0].Down = false
	r.run(time.Minute)
	if r.sink.count() != 2 || r.sink.last() != "The Pizza printer is working again." {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
}

func TestPrinterBlipNeverReachesTheOwner(t *testing.T) {
	r := newRig(t, "")
	for i := 0; i < 20; i++ { // 20 blips of 2 minutes each
		r.printers = []PrinterState{{Name: "Grill", Down: true, DownSince: r.now}}
		r.run(2 * time.Minute)
		r.printers = []PrinterState{{Name: "Grill"}}
		r.run(time.Minute)
	}
	if r.sink.count() != 0 {
		t.Fatalf("flapping reached the owner: %v", r.sink.msgs)
	}
}

func TestFlappingPrinterCooldown(t *testing.T) {
	r := newRig(t, "")
	down := func() { r.printers = []PrinterState{{Name: "Pizza", Down: true, DownSince: r.now}} }
	down()
	r.run(6 * time.Minute) // down told
	r.printers = []PrinterState{{Name: "Pizza"}}
	r.run(time.Minute) // back told
	down()
	r.run(8 * time.Minute) // down again within 30 min of the first: held off
	if r.sink.count() != 2 {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
	r.run(20 * time.Minute) // still down once the cooldown is over: told again
	if r.sink.count() != 3 || !strings.Contains(r.sink.last(), "off or out of paper") {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
}

func TestNothingAtNightOutsideOpeningHours(t *testing.T) {
	r := newRig(t, "")
	r.now = at(12, 23, 30)
	r.printers = []PrinterState{{Name: "Pizza", Down: true, DownSince: r.now}}
	r.link = Link{Linked: true}
	r.waiting = []time.Time{r.now, r.now}
	r.w.NotPrinted("o1", "#1104", r.now)
	r.run(2 * time.Hour)
	if r.sink.count() != 0 {
		t.Fatalf("sent at night: %v", r.sink.msgs)
	}
	// Still down when the store opens: told then.
	r.now = at(13, 11, 0)
	r.printers[0].DownSince = at(12, 23, 30)
	r.link = Link{Linked: true, Connected: true}
	r.waiting = nil
	r.run(time.Minute)
	if r.sink.count() != 1 || !strings.Contains(r.sink.last(), "Pizza printer") {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
}

func TestFallbackHoursWhenUnknown(t *testing.T) {
	sc := &storeconfig.StoreContext{}
	sc.Location.Timezone = "America/New_York"
	for _, c := range []struct {
		t    time.Time
		open bool
	}{{at(12, 8, 59), false}, {at(12, 9, 0), true}, {at(12, 23, 0), true}, {at(13, 1, 59), true}, {at(13, 2, 0), false}, {at(13, 5, 0), false}} {
		if OpenAt(sc, c.t) != c.open {
			t.Fatalf("%v open = %v", c.t, !c.open)
		}
	}
}

func TestOfflineOncePerOutageAndRecovery(t *testing.T) {
	r := newRig(t, "")
	r.link = Link{Linked: true}
	r.run(4 * time.Minute)
	if r.sink.count() != 0 {
		t.Fatalf("too soon: %v", r.sink.msgs)
	}
	r.run(time.Hour)
	if r.sink.count() != 1 || !strings.Contains(r.sink.last(), "offline for 5 minutes") {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
	r.link.Connected = true
	r.run(time.Minute)
	if r.sink.count() != 2 || r.sink.last() != "The store is back online. New online orders are coming in again." {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
	// A short drop: nothing.
	r.link.Connected = false
	r.run(3 * time.Minute)
	r.link.Connected = true
	r.run(time.Minute)
	// A revoked key is the developer's: nothing.
	r.link = Link{Linked: true, Refused: true}
	r.run(time.Hour)
	if r.sink.count() != 2 {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
}

func TestOfflineLeftToMainWhenMainPagesTheOwner(t *testing.T) {
	r := newRig(t, "")
	r.byMain = true
	r.link = Link{Linked: true}
	r.run(time.Hour)
	r.link.Connected = true
	r.run(time.Minute)
	if r.sink.count() != 0 {
		t.Fatalf("main pages the owner for an outage: one alert, never two; msgs = %v", r.sink.msgs)
	}
	// The other owner alerts are untouched.
	r.waiting = []time.Time{r.now.Add(-20 * time.Minute)}
	r.run(Tick)
	if r.sink.count() != 1 || !strings.Contains(r.sink.last(), "waiting") {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
}

func TestOfflineToldBeforeMainTookOverStillGetsItsRecovery(t *testing.T) {
	r := newRig(t, "")
	r.link = Link{Linked: true}
	r.run(10 * time.Minute)
	if r.sink.count() != 1 {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
	// The reconnect brings a store_config that says main pages from now on.
	r.byMain = true
	r.link.Connected = true
	r.run(time.Minute)
	if r.sink.count() != 2 || !strings.Contains(r.sink.last(), "back online") {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
	r.link.Connected = false
	r.run(time.Hour)
	if r.sink.count() != 2 {
		t.Fatalf("the next outage is main's: msgs = %v", r.sink.msgs)
	}
}

func TestOfflineSentWhenPushoverIsReachableAgain(t *testing.T) {
	r := newRig(t, "")
	r.link = Link{Linked: true}
	r.sink.fail = true // the store's internet is down: Pushover too
	r.run(10 * time.Minute)
	r.sink.fail = false
	r.run(Tick)
	if r.sink.count() != 1 {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
}

func TestWaitingOrdersOncePerIncident(t *testing.T) {
	r := newRig(t, "")
	r.waiting = []time.Time{r.now}
	r.run(10 * time.Minute) // one order, 10 min: not yet
	if r.sink.count() != 0 {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
	r.waiting = append(r.waiting, r.now.Add(-5*time.Minute)) // a second one over 5 min
	r.run(20 * time.Minute)
	if r.sink.count() != 1 || !strings.HasPrefix(r.sink.last(), "2 orders are waiting to be accepted") {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
	r.waiting = nil
	r.run(time.Minute)
	// One order waiting 15 min is an incident too; within the cooldown it
	// waits, then goes out.
	r.waiting = []time.Time{r.now.Add(-15 * time.Minute)}
	r.run(Tick)
	if r.sink.count() != 1 {
		t.Fatalf("within cooldown: %v", r.sink.msgs)
	}
	r.run(15 * time.Minute)
	if r.sink.count() != 2 || !strings.HasPrefix(r.sink.last(), "An order has been waiting") {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
}

func TestNotPrintedOncePerOrder(t *testing.T) {
	r := newRig(t, "")
	r.w.NotPrinted("o1", "#1104", r.now)
	r.w.NotPrinted("o1", "#1104", r.now)
	r.run(Tick)
	r.w.NotPrinted("o1", "#1104", r.now) // a reprint failing again
	r.run(Tick)
	if r.sink.count() != 1 || r.sink.last() != "Order #1104 did not print on any printer. Check the printers, then print it again from the tablet." {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
	r.w.NotPrinted("o2", "#1105", r.now)
	r.run(Tick)
	if r.sink.count() != 2 {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
}

func TestCapsAndOneSummary(t *testing.T) {
	r := newRig(t, "")
	for i := 0; i < 10; i++ {
		r.w.NotPrinted(string(rune('a'+i)), "#"+string(rune('0'+i)), r.now)
	}
	r.run(Tick)
	if r.sink.count() != 6 {
		t.Fatalf("hour cap: %d sent", r.sink.count())
	}
	r.run(59 * time.Minute)
	if r.sink.count() != 6 {
		t.Fatalf("summary before the hour: %v", r.sink.msgs)
	}
	r.run(2 * time.Minute)
	if r.sink.count() != 7 || !strings.HasPrefix(r.sink.last(), "To keep messages few, 4 more problems waited: order #6 did not print") {
		t.Fatalf("summary = %q", r.sink.last())
	}
	r.run(2 * time.Hour) // one summary, not a summary a tick
	if r.sink.count() != 7 {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
}

func TestDailyCap(t *testing.T) {
	r := newRig(t, "")
	n := 0
	for h := 0; h < 10; h++ { // 10 hours of 3 new problems an hour
		for i := 0; i < 3; i++ {
			n++
			r.w.NotPrinted(time.Duration(n).String(), "#1", r.now)
		}
		r.run(time.Hour)
	}
	if r.sink.count() > 20 {
		t.Fatalf("daily cap broken: %d", r.sink.count())
	}
	if r.pol.SentCount(r.now) != r.sink.count() {
		t.Fatalf("sent count %d vs %d", r.pol.SentCount(r.now), r.sink.count())
	}
}

func TestCloseBatchOnceADayAndStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, dir)
	r.now = at(13, 9, 0)
	r.batch = &Batch{OpenedAt: at(12, 2, 10), Orders: 40} // not closed last night
	r.run(time.Hour)                                      // 10:00 check, but the store opens at 11
	if r.sink.count() != 0 {
		t.Fatalf("before opening: %v", r.sink.msgs)
	}
	r.run(90 * time.Minute)
	if r.sink.count() != 1 || !strings.Contains(r.sink.last(), "tap Close batch") {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
	// A restart the same day: no second message.
	r2 := newRig(t, dir)
	r2.now, r2.batch = r.now, r.batch
	r2.run(time.Hour)
	if r2.sink.count() != 0 {
		t.Fatalf("after restart: %v", r2.sink.msgs)
	}
}

func TestCloseBatchDoneLastNight(t *testing.T) {
	r := newRig(t, "")
	r.now = at(13, 11, 0)
	r.batch = &Batch{OpenedAt: at(13, 0, 30), Orders: 2} // closed after midnight, two orders today
	r.run(time.Hour)
	r.batch = &Batch{OpenedAt: at(10, 23, 0), Orders: 0} // nothing to close
	r.run(time.Hour)
	if r.sink.count() != 0 {
		t.Fatalf("msgs = %v", r.sink.msgs)
	}
}

func TestSwitchedOffAndSettings(t *testing.T) {
	r := newRig(t, "")
	r.cfg[KeyEnabled] = false
	r.w.NotPrinted("o1", "#1", r.now)
	r.run(Tick)
	if r.sink.count() != 0 {
		t.Fatal("off but sent")
	}
	var sc storeconfig.StoreContext
	sc.Location.Config = map[string]any{KeyCooldownMinutes: float64(5), KeyPrinterDownMinutes: float64(500),
		KeyMaxPerHour: float64(3), KeyCloseCheckHour: float64(8)}
	s := SettingsFrom(&sc)
	if s.Cooldown != MinCooldown || s.PrinterDown != 120*time.Minute || s.MaxPerHour != 3 || s.CloseCheckHour != 8 || !s.Enabled {
		t.Fatalf("settings = %+v", s)
	}
	if d := SettingsFrom(nil); d != Defaults() {
		t.Fatalf("defaults = %+v", d)
	}
}

// The owner's words: no technical terms.
func TestStatePath(t *testing.T) {
	if got := StatePath("/var/lib/quicc-agent/agent.db"); got != "/var/lib/quicc-agent/agent.owner-alerts.json" {
		t.Fatal(got)
	}
	if got := StatePath("sql/agent-lombardis.db"); got != "sql/agent-lombardis.owner-alerts.json" {
		t.Fatal(got)
	}
}

func TestWordingIsPlain(t *testing.T) {
	bad := regexp.MustCompile(`(?i)\b(pi|sse|main|otlp|rpc|api|agent|probe|unreachable|http|ip|tcp|sqlite|batch \d|error)\b`)
	alerts := []Alert{PrinterDown("Pizza"), PrinterBack("Pizza"), NotPrinted("x", "#1104"), Offline(7 * time.Minute),
		Online(), Waiting(3, 12*time.Minute), Waiting(1, 15*time.Minute), CloseBatch("2026-11-12")}
	for _, a := range alerts {
		if bad.MatchString(a.Text) || bad.MatchString(a.Short) || a.Key == "" || a.Short == "" {
			t.Fatalf("not plain: %+v", a)
		}
	}
	if got := PrinterDown("Front Printer").Text; !strings.HasPrefix(got, "The Front Printer is off") {
		t.Fatalf("got %q", got)
	}
	if got := SummaryText([]held{{Key: "a", Short: "x"}}, time.Now()); bad.MatchString(got) {
		t.Fatalf("summary %q", got)
	}
}
