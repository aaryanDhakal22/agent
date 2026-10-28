package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
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

func TestSendFloorGoesToOwnerAndBoss(t *testing.T) {
	n, users := recorder(t)
	if err := n.SendFloor(context.Background(), "NOT ACCEPTED", "siren"); err != nil {
		t.Fatal(err)
	}
	if len(*users) != 2 || (*users)[0] != "owner" || (*users)[1] != "boss" {
		t.Fatalf("users = %v", *users)
	}
}

func TestNoBossKeyMeansOwnerOnly(t *testing.T) {
	n, users := recorder(t)
	n.bossKey = ""
	n.SendFloor(context.Background(), "x", "siren")
	if len(*users) != 1 {
		t.Fatalf("users = %v", *users)
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
	if len(*got) != 3 {
		t.Fatalf("posts = %d", len(*got))
	}
	sys, floor, boss := (*got)[0], (*got)[1], (*got)[2]
	if sys.Get("user") != "owner" || sys.Get("device") != "AaryanPhone" {
		t.Fatalf("system = %v", sys)
	}
	if floor.Get("user") != "owner" || floor.Get("device") != "AaryanPhone,OwnerPhone" || floor.Get("sound") != "siren" {
		t.Fatalf("floor = %v", floor)
	}
	if boss.Get("user") != "boss" || boss.Has("device") {
		t.Fatalf("boss = %v", boss)
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
	if f.Get("device") != "abdul" || f.Get("priority") != "2" || f.Get("retry") != "30" || f.Get("expire") != "600" ||
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
