package health

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestParseThrottled(t *testing.T) {
	for in, want := range map[string]uint64{"0x0": 0, "0x50005": 0x50005, "50000": 0x50000, " 0X1 ": 1} {
		if got, ok := ParseThrottled(in); !ok || got != want {
			t.Errorf("ParseThrottled(%q) = %x, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "unknown", "zz"} {
		if _, ok := ParseThrottled(in); ok {
			t.Errorf("ParseThrottled(%q) should fail", in)
		}
	}
}

func TestParseSystemBits(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		th                             string
		uvNow, uvBoot, thrNow, thrBoot bool
	}{
		{"0x0", false, false, false, false},
		{"0x1", true, false, false, false},
		{"0x10000", false, true, false, false},
		{"0x50005", true, true, true, true},
		{"0x40000", false, false, false, true},
		{"unknown", false, false, false, false},
	}
	for _, c := range cases {
		h := ParseSystem(map[string]any{"throttled": c.th, "at": now.Add(-time.Minute).Format(time.RFC3339)}, now)
		if h.UnderVoltageNow != c.uvNow || h.UnderVoltageSinceBoot != c.uvBoot || h.ThrottledNow != c.thrNow || h.ThrottledSinceBoot != c.thrBoot {
			t.Errorf("%s: got %+v", c.th, h)
		}
		if h.Throttled != c.th || h.FileStale {
			t.Errorf("%s: raw/stale wrong: %+v", c.th, h)
		}
	}
}

func TestParseSystemFields(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	h := ParseSystem(map[string]any{"sd_errors": float64(3), "rootfs_ro": true, "ntp_synced": "no",
		"at": now.Add(-6 * time.Minute).Format(time.RFC3339)}, now)
	if h.SDErrors == nil || *h.SDErrors != 3 || h.RootfsRO == nil || !*h.RootfsRO || h.NTPSynced != "no" {
		t.Errorf("fields: %+v", h)
	}
	if !h.FileStale || !h.FilePresent {
		t.Error("a 6-minute-old file is stale")
	}
	if h := ParseSystem(map[string]any{"at": "garbage"}, now); !h.FileStale {
		t.Error("an unreadable timestamp counts as stale")
	}
	none := ParseSystem(nil, now)
	if !none.FileStale || none.FilePresent || none.SDErrors != nil || none.RootfsRO != nil {
		t.Errorf("no file: %+v", none)
	}
}

func TestSnapshotReadsHealthFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "health.json")
	at := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(f, []byte(`{"at":"`+at+`","sd_errors":0,"throttled":"0x10000","rootfs_ro":false,"ntp_synced":"yes"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := New(Inputs{}, nil, nil, filepath.Join(dir, "agent.db"), "2.0.1", zerolog.Nop())
	h.HealthFile = f
	r := h.Snapshot(context.Background())
	if r.Version != "2.0.1" || r.Pi.FileStale || !r.Pi.UnderVoltageSinceBoot || r.Pi.UnderVoltageNow || r.Pi.NTPSynced != "yes" {
		t.Errorf("snapshot: %+v", r.Pi)
	}
	h.HealthFile = filepath.Join(dir, "missing.json")
	if r := h.Snapshot(context.Background()); !r.Pi.FileStale || r.Pi.FilePresent {
		t.Errorf("missing file: %+v", r.Pi)
	}
}
