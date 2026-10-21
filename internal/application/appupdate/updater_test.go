package appupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
}

func (f *fakeMain) Get(_ context.Context, _, path, _ string) (int, []byte, error) {
	if f.latest == nil {
		return 204, nil, nil
	}
	b, _ := json.Marshal(f.latest)
	return 200, b, nil
}
func (f *fakeMain) Download(_ context.Context, _, path, _ string) (int, []byte, error) {
	f.downloads++
	if !strings.HasSuffix(path, "/apk") {
		return 404, nil, nil
	}
	return 200, f.apk, nil
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
