package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
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
