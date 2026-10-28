package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/infra/notify"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/migrate"
	"quiccpos/agent/internal/observability"
)

// fakePushover records posted forms and answers with status/body.
type fakePushover struct {
	mu     sync.Mutex
	forms  []url.Values
	status int
	body   string
}

func (f *fakePushover) posts() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.forms...)
}

// newRouteService is newPoolService with a notifier pointed at a fake
// Pushover (the admin device set, so alerts are scoped).
func newRouteService(t *testing.T) (*Service, *repositories.OrderRepository, chan ssebroker.Event, *fakePushover) {
	t.Helper()
	ctx := context.Background()
	fake := &fakePushover{status: 200, body: `{"status":1}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		fake.mu.Lock()
		fake.forms = append(fake.forms, r.Form)
		status, body := fake.status, fake.body
		fake.mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PUSHOVER_API_URL", srv.URL)
	n := notify.NewNotifier("app", "user", "")
	n.SetAdminDevices([]string{"AdminPhone"})

	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repo := repositories.NewOrderRepository(db, zerolog.Nop())
	meters, _ := observability.NewMeters()
	broker := ssebroker.New()
	events, _ := broker.Subscribe()
	print := func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{Printed: []printer.TicketResult{{Kind: printer.TicketFull}}}, nil
	}
	svc := NewService(repo, repositories.NewSettingsRepository(db, zerolog.Nop()), print, n, broker, zerolog.Nop(), meters)
	noAutoAssign(t, repo)
	svc.SetGeo(GeoDeps{Floor: func() (*FloorStore, *FloorMap) { return &FloorStore{Lat: 39.40000, Lng: -76.60000}, nil }})
	return svc, repo, events, fake
}

// routeEvent waits for SSE driver.route.
func routeEvent(t *testing.T, events chan ssebroker.Event) DriverRoute {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == "driver.route" {
				var r DriverRoute
				if err := json.Unmarshal(ev.Data, &r); err != nil {
					t.Fatal(err)
				}
				return r
			}
		case <-deadline:
			t.Fatal("no driver.route event")
		}
	}
}

func TestSendOutPushesTheRouteInAutoOrder(t *testing.T) {
	ctx := context.Background()
	svc, repo, events, fake := newRouteService(t)
	ann, _ := svc.CreateDriver(ctx, "Ann", "")
	if err := svc.SetDriverDevice(ctx, ann.ID, "ann_phone"); err != nil {
		t.Fatal(err)
	}
	svc.ClockIn(ctx, ann.ID)
	// far placed first, near second: the tablet's pool order is far → near,
	// AUTO drives near → far.
	far := readyDelivery(t, svc, 20*time.Minute)
	near := readyDelivery(t, svc, 10*time.Minute)
	locate(t, svc, far, 39.46000, -76.60000, 900)
	locate(t, svc, near, 39.41000, -76.60000, 120, order.GeoTime{Other: far, ToS: ptr[int64](800), FromS: ptr[int64](800)})
	if _, err := svc.Pool(ctx, []string{far, near}, ann.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendOut(ctx, ann.ID, SendOutRequest{}); err != nil {
		t.Fatal(err)
	}
	ev := routeEvent(t, events)
	svc.WaitBackground()
	if !ev.OK || ev.Stops != 2 || ev.DriverName != "Ann" {
		t.Fatalf("event = %+v", ev)
	}
	if got := runSeqs(t, repo, near, far); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("run order must be AUTO's (near first): %v", got)
	}
	posts := fake.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %d", len(posts))
	}
	f := posts[0]
	if f.Get("device") != "ann_phone" || f.Get("priority") != "2" || f.Get("retry") != "30" || f.Get("expire") != "600" ||
		f.Get("sound") != "persistent" || f.Get("title") != "Route · 2 stops" {
		t.Fatalf("form = %v", f)
	}
	link := f.Get("url")
	want := "waypoints=39.41000%2C-76.60000%7C39.46000%2C-76.60000"
	if !strings.Contains(link, want) || !strings.Contains(link, "origin=39.40000%2C-76.60000") ||
		!strings.Contains(link, "destination=39.40000%2C-76.60000") {
		t.Fatalf("link = %s", link)
	}
}

func runSeqs(t *testing.T, repo *repositories.OrderRepository, ids ...string) []int64 {
	t.Helper()
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		so, err := repo.GetByID(context.Background(), uuid.MustParse(id))
		if err != nil {
			t.Fatal(err)
		}
		if so.Status.RunSeq == nil {
			out = append(out, 0)
		} else {
			out = append(out, *so.Status.RunSeq)
		}
	}
	return out
}

func TestSendOutWithoutDeviceSendsNothing(t *testing.T) {
	ctx := context.Background()
	svc, _, _, fake := newRouteService(t)
	bob, _ := svc.CreateDriver(ctx, "Bob", "")
	svc.ClockIn(ctx, bob.ID)
	a := readyDelivery(t, svc, 10*time.Minute)
	svc.Pool(ctx, []string{a}, bob.ID)
	if _, err := svc.SendOut(ctx, bob.ID, SendOutRequest{}); err != nil {
		t.Fatal(err)
	}
	svc.WaitBackground()
	if n := len(fake.posts()); n != 0 {
		t.Fatalf("posts = %d", n)
	}
}

func TestSendOutRouteFailureTellsTheTablet(t *testing.T) {
	ctx := context.Background()
	svc, _, events, fake := newRouteService(t)
	fake.status, fake.body = 400, `{"errors":["device name is not valid for user"],"status":0}`
	cat, _ := svc.CreateDriver(ctx, "Cat", "")
	svc.SetDriverDevice(ctx, cat.ID, "wrong")
	svc.ClockIn(ctx, cat.ID)
	a := readyDelivery(t, svc, 10*time.Minute)
	svc.Pool(ctx, []string{a}, cat.ID)
	if _, err := svc.SendOut(ctx, cat.ID, SendOutRequest{}); err != nil {
		t.Fatalf("Send Out must still succeed: %v", err)
	}
	ev := routeEvent(t, events)
	svc.WaitBackground()
	if ev.OK || ev.Code != "device_not_found" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestTestRoute(t *testing.T) {
	ctx := context.Background()
	svc, _, _, fake := newRouteService(t)
	d, _ := svc.CreateDriver(ctx, "Dee", "")
	if err := svc.TestRoute(ctx, d.ID); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("no device: %v", err)
	}
	if err := svc.SetDriverDevice(ctx, d.ID, "bad name"); !errors.Is(err, ErrBadDevice) {
		t.Fatalf("bad device: %v", err)
	}
	svc.SetDriverDevice(ctx, d.ID, "dee")
	if err := svc.TestRoute(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if f := fake.posts()[0]; f.Get("device") != "dee" || !strings.Contains(f.Get("url"), "origin=") {
		t.Fatalf("form = %v", f)
	}
	fake.status, fake.body = 400, `{"errors":["device name is not valid for user"],"status":0}`
	var re *RouteError
	if err := svc.TestRoute(ctx, d.ID); !errors.As(err, &re) || re.Code != "device_not_found" {
		t.Fatalf("unknown device: %v", err)
	}
	if err := svc.TestRoute(ctx, uuid.NewString()); !errors.Is(err, order.ErrDriverNotFound) {
		t.Fatalf("unknown driver: %v", err)
	}
	if err := svc.SetDriverDevice(ctx, d.ID, ""); err != nil {
		t.Fatal(err)
	}
	ds, _ := svc.ListDrivers(ctx)
	if ds[0].PushoverDevice != "" {
		t.Fatalf("cleared device: %+v", ds[0])
	}
}

func TestRouteLink(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	store := &FloorStore{Lat: 39.4, Lng: -76.6}
	pin := RouteStop{Label: "#1", Lat: f(39.41), Lng: f(-76.61)}
	addr := RouteStop{Label: "#2", Addr: &order.Address{Line1: "4108 Old Court Rd", City: "Towson", State: "MD", PostalCode: "21204"}}
	link, n := RouteLink(store, []RouteStop{pin, addr})
	if n != 2 || !strings.Contains(link, "waypoints=39.41000%2C-76.61000%7C4108+Old+Court+Rd+Towson+MD+21204") {
		t.Fatalf("link = %s (%d)", link, n)
	}
	// No store pin: from the phone's location to the last stop.
	link, _ = RouteLink(nil, []RouteStop{pin, addr})
	if strings.Contains(link, "origin=") || !strings.Contains(link, "destination=4108+Old+Court+Rd") ||
		!strings.Contains(link, "waypoints=39.41000%2C-76.61000&") && !strings.HasSuffix(link, "waypoints=39.41000%2C-76.61000") {
		t.Fatalf("no store link = %s", link)
	}
	// 12 stops: Google's 9 waypoints.
	var many []RouteStop
	for i := 0; i < 12; i++ {
		many = append(many, RouteStop{Label: fmt.Sprintf("#%d", i), Lat: f(39.4 + float64(i)/100), Lng: f(-76.6)})
	}
	link, n = RouteLink(store, many)
	if n != 9 || len(link) > 512 {
		t.Fatalf("9 stops: n=%d len=%d", n, len(link))
	}
	_, text := routeText(many, n)
	if !strings.HasSuffix(text, "(+3 more on the ticket)") {
		t.Fatalf("text = %q", text)
	}
	// Long addresses shrink, then drop, to stay within 512.
	var long []RouteStop
	for i := 0; i < 9; i++ {
		long = append(long, RouteStop{Label: "#", Addr: &order.Address{Line1: fmt.Sprintf("%d Very Long Street Name Boulevard Extension", 1000+i),
			City: "Towson", State: "MD", PostalCode: "21204"}})
	}
	link, n = RouteLink(store, long)
	if len(link) > 512 || n == 0 || n > 9 {
		t.Fatalf("long: n=%d len=%d", n, len(link))
	}
	if title, _ := routeText([]RouteStop{pin}, 1); title != "Route · 1 stop" {
		t.Fatalf("title = %q", title)
	}
}

func TestMergeOrder(t *testing.T) {
	got := mergeOrder([]string{"b", "x"}, []string{"a", "b", "c"}, []string{"a", "b", "c", "d"})
	if fmt.Sprint(got) != "[b a c d]" {
		t.Fatalf("got %v", got)
	}
}
