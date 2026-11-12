package appupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

type memStore struct{ raw string }

func (m *memStore) GetAppRelease(context.Context) (string, error) { return m.raw, nil }
func (m *memStore) SetAppRelease(_ context.Context, raw string) error {
	m.raw = raw
	return nil
}

type fakeMain struct {
	latest    *Release
	apk       []byte
	downloads int
	queries   []string
}

func (f *fakeMain) Get(_ context.Context, _, path, q string) (int, []byte, error) {
	f.queries = append(f.queries, q)
	if f.latest == nil {
		return 204, nil, nil
	}
	b, _ := json.Marshal(f.latest)
	return 200, b, nil
}
func (f *fakeMain) DownloadTo(_ context.Context, _, path, q string, w io.Writer, limit int64) (int, int64, error) {
	f.downloads++
	f.queries = append(f.queries, q)
	if !strings.HasSuffix(path, "/apk") {
		return 404, 0, nil
	}
	body := f.apk
	if int64(len(body)) > limit {
		body = body[:limit+1]
		n, _ := w.Write(body)
		return 200, int64(n), fmt.Errorf("body larger than %d bytes", limit)
	}
	n, err := w.Write(body)
	return 200, int64(n), err
}

func release(code int64, apk []byte) *Release {
	sum := sha256.Sum256(apk)
	return &Release{App: App, VersionCode: code, VersionName: "1.0", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(apk))}
}

func TestCheckCachesNewerReleases(t *testing.T) {
	dir := t.TempDir()
	store, main := &memStore{}, &fakeMain{}
	key := "k"
	u := New(dir, store, main, func() string { return key }, zerolog.Nop())
	ctx := context.Background()

	if err := u.Check(ctx); err != nil || u.Current() != nil {
		t.Fatalf("no release: %v %v", err, u.Current())
	}
	v1 := []byte("PK\x03\x04one")
	main.latest, main.apk = release(10, v1), v1
	if err := u.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if c := u.Current(); c == nil || c.VersionCode != 10 {
		t.Fatalf("current = %v", c)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "quibase-10.apk")); string(b) != string(v1) {
		t.Fatal("apk not written")
	}
	// Same version: no new download.
	if err := u.Check(ctx); err != nil || main.downloads != 1 {
		t.Fatalf("re-check downloaded again: %v %d", err, main.downloads)
	}
	// Corrupted download is rejected.
	main.latest, main.apk = release(11, []byte("PK\x03\x04two")), []byte("PK\x03\x04tw0")
	if err := u.Check(ctx); err == nil || u.Current().VersionCode != 10 {
		t.Fatalf("bad checksum accepted: %v", err)
	}
	// v12, then v13: only the current + previous APK stay on disk.
	for _, code := range []int64{12, 13} {
		apk := []byte("PK\x03\x04v" + string(rune('0'+code-10)))
		main.latest, main.apk = release(code, apk), apk
		if err := u.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "quibase-*.apk"))
	if len(files) != 2 {
		t.Fatalf("kept files = %v", files)
	}
	// A restart restores the cache from the store.
	u2 := New(dir, store, main, func() string { return key }, zerolog.Nop())
	u2.Load(ctx)
	if c := u2.Current(); c == nil || c.VersionCode != 13 {
		t.Fatalf("after load = %v", c)
	}
	// Unprovisioned: never calls main.
	key = ""
	main.downloads = 0
	main.latest = release(14, []byte("x"))
	if err := u.Check(ctx); err != nil || main.downloads != 0 {
		t.Fatalf("unprovisioned check: %v %d", err, main.downloads)
	}
}

// The terminal's APK (contract §2.10w): its own app name with main, its own
// files and store, so the two caches never touch each other; a newly cached
// release runs the OnCached hook (the Pi's terminal.app.available).
func TestTerminalAppCacheIsSeparate(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	tabStore, termStore := &memStore{}, &memStore{}
	tabMain, termMain := &fakeMain{}, &fakeMain{}
	tab := New(dir, tabStore, tabMain, func() string { return "k" }, zerolog.Nop())
	term := NewFor(AppTerminal, dir, termStore, termMain, func() string { return "k" }, zerolog.Nop())
	var cached []int64
	term.OnCached(func(r Release) { cached = append(cached, r.VersionCode) })
	a, b := []byte("PK\x03\x04tablet"), []byte("PK\x03\x04terminal")
	tabMain.latest, tabMain.apk = release(20, a), a
	termMain.latest, termMain.apk = release(30, b), b
	termMain.latest.App = AppTerminal
	if err := tab.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := term.Check(ctx); err != nil {
		t.Fatal(err)
	}
	for _, q := range termMain.queries {
		if q != "app=terminal" {
			t.Fatalf("terminal asked main %q", q)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "terminal-30.apk")); string(got) != string(b) || term.Path(*term.Current()) != filepath.Join(dir, "terminal-30.apk") {
		t.Fatal("terminal apk not written")
	}
	if tab.Current().VersionCode != 20 || termStore.raw == "" || tabStore.raw == termStore.raw || len(cached) != 1 || cached[0] != 30 {
		t.Fatalf("caches mixed: %v %v %v", tab.Current(), termStore.raw, cached)
	}
	// Pruning one app keeps the other's files.
	for _, code := range []int64{31, 32, 33} {
		apk := []byte(fmt.Sprintf("PK\x03\x04t%d", code))
		termMain.latest, termMain.apk = release(code, apk), apk
		if err := term.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "quibase-20.apk")); err != nil {
		t.Fatal("the tablet APK was pruned by the terminal's cache")
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "terminal-*.apk")); len(files) != 2 {
		t.Fatalf("terminal files %v", files)
	}
}
