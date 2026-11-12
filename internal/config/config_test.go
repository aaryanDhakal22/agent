package config

import (
	"testing"
	"time"
)

func TestDevTimersOnlyOnDev(t *testing.T) {
	t.Parallel()
	got, refused := parseDevTimers("dev", "backup=15s, retention=30s")
	if refused != "" || got["backup"] != 15*time.Second || got["retention"] != 30*time.Second {
		t.Fatalf("dev: %v %q", got, refused)
	}
	for _, env := range []string{"staging", "prod", ""} {
		if got, refused := parseDevTimers(env, "backup=15s"); got != nil || refused == "" {
			t.Fatalf("%q must refuse: %v %q", env, got, refused)
		}
	}
	for _, bad := range []string{"backup=0.5s", "backup=soon", "autoclose=5s", "backup"} {
		if got, refused := parseDevTimers("dev", bad); got != nil || refused == "" {
			t.Fatalf("%q must refuse: %v %q", bad, got, refused)
		}
	}
	if got, refused := parseDevTimers("prod", ""); got != nil || refused != "" {
		t.Fatalf("unset: %v %q", got, refused)
	}
}
