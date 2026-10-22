package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"quiccpos/agent/internal/application/mapfile"

	"github.com/rs/zerolog"
)

type mapState struct{ raw string }

func (m *mapState) GetMapFile(context.Context) (string, error)     { return m.raw, nil }
func (m *mapState) SetMapFile(_ context.Context, raw string) error { m.raw = raw; return nil }

func TestMapFileServedWithRange(t *testing.T) {
	s, h, _ := diagServer(t)
	// No downloader / nothing downloaded: 404, and no auth needed either way.
	if rec := call(h, "GET", "/api/map.pmtiles", "", ""); rec.Code != http.StatusNotFound || code(rec) != "not_found" {
		t.Fatalf("no map: %d %s", rec.Code, rec.Body)
	}
	dir := t.TempDir()
	body := []byte("0123456789PMTILES")
	sum := sha256.Sum256(body)
	path := filepath.Join(dir, mapfile.FileName)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	st := &mapState{raw: `{"version":"20261022","sha256":"` + hex.EncodeToString(sum[:]) + `","bbox":[1,2,3,4]}`}
	d := mapfile.New(path, st, zerolog.Nop())
	d.Load(context.Background())
	s.SetMap(d)
	if rec := call(h, "GET", "/api/map.pmtiles", "", ""); rec.Code != http.StatusOK || rec.Body.String() != string(body) ||
		rec.Header().Get("Cache-Control") != "max-age=86400" {
		t.Fatalf("full: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	req := httptest.NewRequest("GET", "/api/map.pmtiles", nil)
	req.Header.Set("Range", "bytes=10-16")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "PMTILES" {
		t.Fatalf("range: %d %q", rec.Code, rec.Body)
	}
}

func TestPoolRoutesNeedADevice(t *testing.T) {
	_, h, key := diagServer(t)
	for _, rt := range []struct{ method, path string }{
		{"POST", "/api/deliveries/pool"}, {"POST", "/api/drivers/x/send-out"}, {"POST", "/api/deliveries/auto-plan"},
		{"POST", "/api/deliveries/auto-plan/x/confirm"}, {"GET", "/api/drivers/x/pool/trip"}, {"POST", "/api/orders/x/pin"},
		{"PUT", "/api/settings/deliveries"},
	} {
		if rec := call(h, rt.method, rt.path, "", "{}"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key: %d", rt.method, rt.path, rec.Code)
		}
	}
	// With a key: AUTO with nobody clocked in.
	if rec := call(h, "POST", "/api/deliveries/auto-plan", key, "{}"); rec.Code != http.StatusConflict || code(rec) != "no_drivers" {
		t.Fatalf("auto-plan: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/drivers/nope/send-out", key, `{"mark_ready":true}`); rec.Code != http.StatusNotFound || code(rec) != "driver_not_found" {
		t.Fatalf("send-out: %d %s", rec.Code, rec.Body)
	}
}
