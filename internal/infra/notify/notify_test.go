package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

func recorder(t *testing.T) (*Notifier, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var users []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		users = append(users, r.Form.Get("user"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	n := NewNotifier("app", "owner", "boss")
	n.pushoverURL = srv.URL
	n.sendAllowed = true // these tests are about scoping, not the prod-only guard
	return n, &users
}

func TestSendGoesToTheOwnerOnly(t *testing.T) {
	n, users := recorder(t)
	if err := n.Send(context.Background(), "Agent started", "classical"); err != nil {
		t.Fatal(err)
	}
	if len(*users) != 1 || (*users)[0] != "owner" {
		t.Fatalf("users = %v", *users)
	}
}

// 2.5.2: floor problems go to the admin only; the owner + boss get SendOwner.
func TestSendFloorIsAdminOnly(t *testing.T) {
	n, users := recorder(t)
	if err := n.SendFloor(context.Background(), "NOT ACCEPTED", "siren"); err != nil {
		t.Fatal(err)
	}
	if len(*users) != 1 || (*users)[0] != "owner" {
		t.Fatalf("users = %v", *users)
	}
}

func TestSendOwnerGoesToOwnerDevicesAndBoss(t *testing.T) {
	n, got := forms(t, 200, `{"status":1}`)
	n.SetAdminDevices([]string{"AaryanPhone"})
	if n.SendOwner(context.Background(), "Lombardi's", "x", ""); len(*got) != 1 || (*got)[0].Get("user") != "boss" ||
		(*got)[0].Has("device") || (*got)[0].Get("title") != "Lombardi's" {
		t.Fatalf("boss only = %v", *got)
	}
	n.SetOwnerDevices([]string{"OwnerPhone"})
	n.SendOwner(context.Background(), "", "y", "")
	if len(*got) != 3 || (*got)[1].Get("user") != "owner" || (*got)[1].Get("device") != "OwnerPhone" || (*got)[2].Get("user") != "boss" {
		t.Fatalf("owner devices + boss = %v", *got)
	}
	if !n.OwnerConfigured() {
		t.Fatal("OwnerConfigured")
	}
}

// Never the whole account: no owner devices and no separate boss key = no
// owner message (drivers' phones share the account).
func TestSendOwnerNeverUnscoped(t *testing.T) {
	n, got := forms(t, 200, `{"status":1}`)
	n.SetKeys("app", "owner", "owner") // boss = the shared account
	n.SendOwner(context.Background(), "", "x", "")
	n.SetKeys("app", "owner", "")
	n.SendOwner(context.Background(), "", "x", "")
	if len(*got) != 0 || n.OwnerConfigured() {
		t.Fatalf("sent %v", *got)
	}
	// Outside prod, nothing until admin devices are known (2.4.1).
	n.SetKeys("app", "owner", "boss")
	n.SetStrictScope(true)
	n.SendOwner(context.Background(), "", "x", "")
	if len(*got) != 0 {
		t.Fatalf("strict sent %v", *got)
	}
}

// forms records every posted form; status/body answer each post.
func forms(t *testing.T, status int, body string) (*Notifier, *[]url.Values) {
	t.Helper()
	var mu sync.Mutex
	var got []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		got = append(got, r.Form)
		mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	n := NewNotifier("app", "owner", "boss")
	n.pushoverURL = srv.URL
	n.sendAllowed = true // these tests are about scoping, not the prod-only guard
	return n, &got
}

func TestUnscopedWithoutAdminDevices(t *testing.T) {
	n, got := forms(t, 200, `{"status":1}`)
	n.SetOwnerDevices([]string{"OwnerPhone"}) // no admin devices: still the whole account
	n.Send(context.Background(), "Agent started", "classical")
	n.SendFloor(context.Background(), "NOT ACCEPTED", "siren")
	for _, f := range *got {
		if f.Has("device") {
			t.Fatalf("device set without admin devices: %v", f)
		}
	}
}

func TestScopedAudiences(t *testing.T) {
	n, got := forms(t, 200, `{"status":1}`)
	n.SetAdminDevices([]string{"AaryanPhone"})
	n.SetOwnerDevices([]string{"OwnerPhone", "AaryanPhone"})
	n.Send(context.Background(), "Agent started", "classical")
	n.SendFloor(context.Background(), "NOT ACCEPTED", "siren")
	if len(*got) != 2 {
		t.Fatalf("posts = %d", len(*got))
	}
	sys, floor := (*got)[0], (*got)[1]
	if sys.Get("user") != "owner" || sys.Get("device") != "AaryanPhone" {
		t.Fatalf("system = %v", sys)
	}
	if floor.Get("user") != "owner" || floor.Get("device") != "AaryanPhone" || floor.Get("sound") != "siren" {
		t.Fatalf("floor = %v", floor)
	}
}

func TestSendRoute(t *testing.T) {
	n, got := forms(t, 200, `{"status":1}`)
	n.SetAdminDevices([]string{"AaryanPhone"})
	link := "https://www.google.com/maps/dir/?api=1&origin=1,2&destination=1,2"
	if err := n.SendRoute(context.Background(), "abdul", "Route · 2 stops", "#1104 → #1106", link); err != nil {
		t.Fatal(err)
	}
	f := (*got)[0]
	if f.Get("device") != "abdul" || f.Get("priority") != "1" || f.Has("retry") || f.Has("expire") ||
		f.Get("sound") != "persistent" || f.Get("url") != link || f.Get("url_title") != "Open route" || f.Get("title") != "Route · 2 stops" {
		t.Fatalf("route = %v", f)
	}
}

func TestSendRouteErrors(t *testing.T) {
	n, _ := forms(t, 400, `{"device":"invalid for current user","errors":["device name is not valid for user"],"status":0,"request":"x"}`)
	err := n.SendRoute(context.Background(), "nobody", "t", "m", "u")
	var api *APIError
	if !errors.As(err, &api) || !api.DeviceUnknown() {
		t.Fatalf("err = %v", err)
	}
	n.SetKeys("", "", "")
	if err := n.SendRoute(context.Background(), "abdul", "t", "m", "u"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured err = %v", err)
	}
	if err := n.Send(context.Background(), "x", "siren"); err != nil {
		t.Fatalf("unconfigured Send must stay silent: %v", err)
	}
}

func TestParseDevices(t *testing.T) {
	got := ParseDevices(" AaryanPhone, tablet ,AaryanPhone,,bad name,toolongtoolongtoolongtoolong")
	if len(got) != 2 || got[0] != "AaryanPhone" || got[1] != "tablet" {
		t.Fatalf("got %v", got)
	}
	if ValidDevice("") || ValidDevice("a b") || !ValidDevice("abdul_2-x") {
		t.Fatal("ValidDevice")
	}
}

// 2.4.1: outside prod, no admin devices = nothing sent (never the drivers'
// phones); a driver message still goes to its one device.
func TestStrictScope(t *testing.T) {
	n, got := forms(t, 200, `{"status":1}`)
	n.SetStrictScope(true)
	n.Send(context.Background(), "Agent started", "classical")
	n.SendFloor(context.Background(), "NOT ACCEPTED", "siren")
	if len(*got) != 0 {
		t.Fatalf("strict scope with no admin devices sent %v", *got)
	}
	n.SetAdminDevices([]string{"AaryanPhone"})
	n.Send(context.Background(), "Agent started", "classical")
	if len(*got) != 1 || (*got)[0].Get("device") != "AaryanPhone" {
		t.Fatalf("scoped send: %v", *got)
	}
	if err := n.SendDriver(context.Background(), "abdul", "Come back to the store", "Your run is on hold."); err != nil {
		t.Fatal(err)
	}
	d := (*got)[len(*got)-1]
	if d.Get("device") != "abdul" || d.Get("priority") != "1" || d.Get("title") != "Come back to the store" || d.Has("sound") {
		t.Fatalf("driver message = %v", d)
	}
}

func TestCooldown(t *testing.T) {
	c := NewCooldown(AdminCooldown)
	t0 := time.Unix(1_700_000_000, 0)
	if !c.Allow("a", t0) || c.Allow("a", t0.Add(14*time.Minute)) || !c.Allow("b", t0) || !c.Allow("a", t0.Add(15*time.Minute)) {
		t.Fatal("cooldown")
	}
	var nilC *Cooldown
	if !nilC.Allow("x", t0) {
		t.Fatal("nil cooldown must allow")
	}
}
