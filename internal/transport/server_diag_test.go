package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/migrate"
	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
)

// diagServer is a Server over a fresh SQLite DB with one paired tablet; it
// returns the mux and that tablet's key.
func diagServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	devices := repositories.NewDeviceRepository(db)
	settings := repositories.NewSettingsRepository(db, zerolog.Nop())
	session := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), func(string) error { return nil })
	s := NewServer(nil, nil, printerApp.NewRegistry(), settings, nil, nil, session, devices, "0", "2.0.1", "test", zerolog.Nop())
	key, _ := newDeviceKey()
	if err := devices.Create(ctx, "dev-1", "Counter", hashDeviceKey(key), key[:11]); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.register(mux)
	return s, mux, key
}

func call(h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set(deviceKeyHeader, key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func code(rec *httptest.ResponseRecorder) string {
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	return b.Code
}

func TestDiagRoutesNeedADevice(t *testing.T) {
	_, h, _ := diagServer(t)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/diag"},
		{"POST", "/api/diag/report"},
		{"POST", "/api/printers/test"},
		{"PATCH", "/api/device/self"},
		{"POST", "/api/device/report-token"},
		{"POST", "/api/pairing/code"},
	} {
		for _, key := range []string{"", "qagt_not-a-real-key"} {
			rec := call(h, rt.method, rt.path, key, "{}")
			if rec.Code != http.StatusUnauthorized || code(rec) != "device_required" {
				t.Errorf("%s %s key=%q: %d %s", rt.method, rt.path, key, rec.Code, rec.Body)
			}
		}
	}
}

func TestPairingCodeWithADeviceKey(t *testing.T) {
	s, h, key := diagServer(t)
	rec := call(h, "POST", "/api/pairing/code", key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Code) != 6 || !s.pairing.consume(body.Code, time.Now()) {
		t.Fatalf("a tablet-minted code must pair: %s", rec.Body)
	}
}

func TestRenameSelf(t *testing.T) {
	s, h, key := diagServer(t)
	for _, bad := range []string{`{"name":""}`, `{"name":"   "}`, `{"name":"` + strings.Repeat("x", 41) + `"}`, `{}`} {
		if rec := call(h, "PATCH", "/api/device/self", key, bad); rec.Code != http.StatusBadRequest || code(rec) != "name_required" {
			t.Errorf("%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	rec := call(h, "PATCH", "/api/device/self", key, `{"name":"  Front counter  "}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"Front counter"`) || !strings.Contains(rec.Body.String(), `"id":"dev-1"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	ds, _ := s.devices.List(context.Background())
	if len(ds) != 1 || ds[0].Name != "Front counter" {
		t.Fatalf("not renamed: %+v", ds)
	}
	// 40 characters (not bytes) is the limit.
	if rec := call(h, "PATCH", "/api/device/self", key, `{"name":"`+strings.Repeat("é", 40)+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("40 characters should be fine: %d %s", rec.Code, rec.Body)
	}
}

func TestDiagSnapshotAndUnlinkedRoutes(t *testing.T) {
	_, h, key := diagServer(t)
	rec := call(h, "GET", "/api/diag", key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "hostname", "store_name", "uptime_s", "main", "health", "problems", "printers",
		"orders", "open_now", "quiet_alert_minutes", "floor_backlog", "acks_pending"} {
		if _, ok := d[k]; !ok {
			t.Errorf("diag missing %q", k)
		}
	}
	if d["main"].(map[string]any)["state"] != "unconfigured" || d["quiet_alert_minutes"].(float64) != 90 || d["open_now"] != nil {
		t.Errorf("diag: %s", rec.Body)
	}
	if h := d["health"].(map[string]any); h["file_stale"] != true {
		t.Errorf("no health file must read as stale: %v", h)
	}

	// Unlinked: report token + report relay answer 503 not_linked.
	if rec := call(h, "POST", "/api/device/report-token", key, ""); rec.Code != http.StatusServiceUnavailable || code(rec) != "not_linked" {
		t.Errorf("report-token: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/diag/report", key, `{"v":1}`); rec.Code != http.StatusServiceUnavailable || code(rec) != "not_linked" {
		t.Errorf("diag/report: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/diag/report", key, `[1,2]`); rec.Code != http.StatusBadRequest {
		t.Errorf("non-object report: %d %s", rec.Code, rec.Body)
	}
	big := `{"log":"` + strings.Repeat("x", maxDiagReport) + `"}`
	if rec := call(h, "POST", "/api/diag/report", key, big); rec.Code != http.StatusRequestEntityTooLarge || code(rec) != "too_large" {
		t.Errorf("oversized report: %d %s", rec.Code, rec.Body)
	}
}

func TestPrinterTestBusyAndUnavailable(t *testing.T) {
	s, h, key := diagServer(t)
	if rec := call(h, "POST", "/api/printers/test", key, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no dispatcher: %d %s", rec.Code, rec.Body)
	}
	meters, err := observability.NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.SetDiag(DiagDeps{Dispatcher: printerApp.NewDispatcher(ctx, nil, s.printers, nil, time.Hour, nil, zerolog.Nop(), meters)})
	rec := call(h, "POST", "/api/printers/test", key, `{"probe_only":true}`)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"results":[]`)) {
		t.Fatalf("empty roster: %d %s", rec.Code, rec.Body)
	}
	s.testMu.Lock()
	rec = call(h, "POST", "/api/printers/test", key, "")
	s.testMu.Unlock()
	if rec.Code != http.StatusConflict || code(rec) != "test_in_progress" {
		t.Fatalf("busy: %d %s", rec.Code, rec.Body)
	}
}

func TestDiagProblems(t *testing.T) {
	sd, ro := int64(2), true
	now := time.Now()
	d := diagResponse{Main: diagMain{State: "connecting"}, Printers: []diagPrinter{{Up: false, LastChecked: &now}}}
	d.Health.SDErrors, d.Health.RootfsRO = &sd, &ro
	d.Health.UnderVoltageSinceBoot, d.Health.NTPSynced, d.Health.FilePresent, d.Health.FileStale = true, "no", true, true
	got := strings.Join(diagProblems(d), ",")
	if got != "sd_errors,rootfs_ro,under_voltage,ntp,health_stale,main_down,printer_down" {
		t.Fatalf("problems = %s", got)
	}
	clean := diagResponse{Main: diagMain{State: "unconfigured"}, Printers: []diagPrinter{{Up: false}}}
	clean.Health.FileStale = true // no file on a dev box: not a problem
	if p := diagProblems(clean); len(p) != 0 {
		t.Fatalf("clean = %v", p)
	}
}
