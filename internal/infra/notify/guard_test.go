package notify

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

// guarded builds a fully configured notifier (keys, admin + owner devices)
// under the given APP_ENV / PUSHOVER_NONPROD, against a counting fake.
func guarded(t *testing.T, appEnv, nonProd string) (*Notifier, *atomic.Int32, *bytes.Buffer) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("APP_ENV", appEnv)
	t.Setenv(NonProdEnvVar, nonProd)
	t.Setenv("PUSHOVER_API_URL", srv.URL)
	n := NewNotifier("app", "user", "boss")
	n.SetAdminDevices([]string{"AdminPhone"})
	n.SetOwnerDevices([]string{"OwnerPhone"})
	var logs bytes.Buffer
	n.SetLogger(zerolog.New(&logs))
	return n, &calls, &logs
}

// sendEverything goes through every public send path once.
func sendEverything(t *testing.T, n *Notifier) {
	t.Helper()
	ctx := context.Background()
	for _, err := range []error{
		n.Send(ctx, "Agent started", "classical"),
		n.SendFloor(ctx, "NOT ACCEPTED", "siren"),
		n.SendOwner(ctx, "Printer down", "The kitchen printer is down.", ""),
		n.SendRoute(ctx, "abdul", "Your route", "2 stops", "https://maps.example"),
		n.SendDriver(ctx, "abdul", "Come back", "Run on hold."),
	} {
		if err != nil {
			t.Fatalf("send: %v", err)
		}
	}
}

// The owner 2026-11-13: staging (and dev, and an unset APP_ENV) never sends
// a Pushover, even with every key configured; it logs what it dropped.
func TestGuardNonProdSendsNothing(t *testing.T) {
	for _, env := range []string{"staging", "dev", "", "Prod", "production"} {
		t.Run("env="+env, func(t *testing.T) {
			n, calls, logs := guarded(t, env, "")
			if n.Sends() {
				t.Fatal("Sends() true outside prod")
			}
			sendEverything(t, n)
			if c := calls.Load(); c != 0 {
				t.Fatalf("APP_ENV=%q made %d Pushover calls", env, c)
			}
			want := "pushover suppressed (APP_ENV="
			if !strings.Contains(logs.String(), want+"staging): Agent started") && env == "staging" {
				t.Fatalf("no suppressed line for Agent started: %s", logs.String())
			}
			if got := strings.Count(logs.String(), want); got < 5 {
				t.Fatalf("%d suppressed lines, want one per message: %s", got, logs.String())
			}
			if !strings.Contains(logs.String(), `"level":"info"`) {
				t.Fatalf("suppressed lines are not info: %s", logs.String())
			}
		})
	}
}

func TestGuardProdSends(t *testing.T) {
	n, calls, logs := guarded(t, "prod", "")
	if !n.Sends() {
		t.Fatal("Sends() false on prod")
	}
	sendEverything(t, n)
	// Send 1, SendFloor 1, SendOwner 2 (owner devices + boss), route 1, driver 1.
	if c := calls.Load(); c != 6 {
		t.Fatalf("prod made %d Pushover calls, want 6", c)
	}
	if strings.Contains(logs.String(), "suppressed") {
		t.Fatalf("prod logged a suppression: %s", logs.String())
	}
}

func TestGuardNonProdEscapeHatch(t *testing.T) {
	for _, env := range []string{"staging", "dev"} {
		n, calls, _ := guarded(t, env, "1")
		sendEverything(t, n)
		if c := calls.Load(); c != 6 {
			t.Fatalf("APP_ENV=%s PUSHOVER_NONPROD=1 made %d calls, want 6", env, c)
		}
	}
	// Only "1" opens it.
	n, calls, _ := guarded(t, "staging", "true")
	sendEverything(t, n)
	if c := calls.Load(); c != 0 {
		t.Fatalf("PUSHOVER_NONPROD=true made %d calls", c)
	}
}
