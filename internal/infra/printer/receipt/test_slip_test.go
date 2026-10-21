package receipt

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/storeconfig"
)

func TestBuildTestSlip_Golden(t *testing.T) {
	sc := storeconfig.StoreContext{Location: storeconfig.LocationContext{Name: "Lombardi's Pizza", Timezone: "America/New_York"}}
	at := time.Date(2026, 10, 20, 19, 4, 0, 0, time.UTC)
	got := BuildTestSlip("Pizza", sc, "Counter tablet", at)
	path := "testdata/test_slip.golden"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("test slip differs from %s (run with -update after checking)\n%s", path, got)
	}
	s := string(got)
	for _, want := range []string{"TEST - Quibase setup", "PIZZA", "Store:   Lombardi's Pizza", "Tablet:  Counter tablet",
		"Printed: Tue Oct 20 3:04 PM", "If you can read this, this printer works."} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestBuildTestSlip_ASCIIOnly(t *testing.T) {
	sc := storeconfig.StoreContext{Tenant: storeconfig.TenantContext{Name: "Café Ünï"}}
	got := BuildTestSlip("Küche", sc, "Tablet ☕", time.Now())
	for i, c := range got {
		if c > 0x7F {
			t.Fatalf("non-ASCII byte 0x%X at %d in %q", c, i, got)
		}
	}
	if !strings.Contains(string(got), "Store:   Caf? ?n?") || !strings.Contains(string(got), "K?CHE") {
		t.Errorf("names should keep their ASCII and mark the rest:\n%s", got)
	}
}
