package mapfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"quiccpos/agent/internal/domain/storeconfig"

	"github.com/rs/zerolog"
)

type memStore struct{ raw string }

func (m *memStore) GetMapFile(context.Context) (string, error)     { return m.raw, nil }
func (m *memStore) SetMapFile(_ context.Context, raw string) error { m.raw = raw; return nil }

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func serve(t *testing.T, body []byte, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDownloadVerifyAndKeep(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	body := []byte("PMTiles v3 fake map")
	var hits atomic.Int32
	srv := serve(t, body, &hits)
	store := &memStore{}
	d := New(filepath.Join(dir, FileName), store, zerolog.Nop())

	// Nothing happens before store_config said what it wants.
	if err := d.Check(ctx); err != nil || hits.Load() != 0 {
		t.Fatalf("before Want: %v hits=%d", err, hits.Load())
	}
	m := &storeconfig.MapFile{URL: srv.URL + "/m.pmtiles", SHA256: sum(body), Version: "20261022", BBox: []float64{-74.2, 40.6, -74.0, 40.7}}
	d.Want(m)
	if err := d.Check(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil || string(got) != string(body) {
		t.Fatalf("file: %v %q", err, got)
	}
	info, _ := d.Current()
	if info == nil || info.Version != "20261022" || len(info.BBox) != 4 {
		t.Fatalf("current: %+v", info)
	}
	// Same version again: no second download (one SD write per version).
	d.Want(m)
	d.Check(ctx)
	if hits.Load() != 1 {
		t.Fatalf("re-downloaded the same version: %d", hits.Load())
	}
	// A restart restores it from the DB (file hash checked).
	d2 := New(filepath.Join(dir, FileName), store, zerolog.Nop())
	d2.Load(ctx)
	if info, _ := d2.Current(); info == nil || info.Version != "20261022" {
		t.Fatalf("after restart: %+v", info)
	}
	// Unpublished: the file goes.
	d2.Want(nil)
	if err := d2.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatal("map file should be removed when unpublished")
	}
	if store.raw != "" {
		t.Fatalf("state not cleared: %q", store.raw)
	}
}

func TestShaMismatchRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	good := []byte("version one")
	var hits atomic.Int32
	srv := serve(t, []byte("tampered bytes"), &hits)
	store := &memStore{}
	d := New(filepath.Join(dir, FileName), store, zerolog.Nop())
	d.Want(&storeconfig.MapFile{URL: srv.URL, SHA256: sum(good), Version: "v1"})
	if err := d.Check(ctx); err != ErrChecksum {
		t.Fatalf("want checksum error, got %v", err)
	}
	if info, _ := d.Current(); info != nil {
		t.Fatalf("a bad download must not become current: %+v", info)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("no file (or temp file) may be left: %v", entries)
	}
	// The same bad publish isn't fetched again right away.
	d.Check(ctx)
	if hits.Load() != 1 {
		t.Fatalf("retried a known-bad publish immediately: %d", hits.Load())
	}
	// A damaged file on disk is dropped at startup.
	os.WriteFile(filepath.Join(dir, FileName), []byte("junk"), 0o644)
	store.raw = `{"version":"v1","sha256":"` + sum(good) + `"}`
	d2 := New(filepath.Join(dir, FileName), store, zerolog.Nop())
	d2.Load(ctx)
	if info, _ := d2.Current(); info != nil {
		t.Fatal("damaged file must not load")
	}
}
