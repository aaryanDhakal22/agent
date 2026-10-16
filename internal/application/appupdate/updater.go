// Package appupdate keeps the newest Quibase (counter tablet app) APK cached
// on the agent so paired tablets can update over the LAN. The console uploads
// a release to main; the agent polls main (on every SSE connect + every few
// minutes), downloads a newer APK, verifies its sha256 and keeps it next to
// the SQLite file. Tablets then GET /api/app/latest + /api/app/apk.
package appupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// App is the only app distributed today.
const App = "quibase"

// Release is a cached app release (main's /store/v1/agent/app/latest shape).
type Release struct {
	App         string    `json:"app"`
	VersionCode int64     `json:"version_code"`
	VersionName string    `json:"version_name"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
}

// Store persists the cached release metadata (settings.app_release).
type Store interface {
	GetAppRelease(ctx context.Context) (string, error)
	SetAppRelease(ctx context.Context, raw string) error
}

// Fetcher is the device-keyed client to main (mainclient.Client).
type Fetcher interface {
	Get(ctx context.Context, key, path, rawQuery string) (int, []byte, error)
	Download(ctx context.Context, key, path, rawQuery string) (int, []byte, error)
}

type Updater struct {
	dir    string
	store  Store
	fetch  Fetcher
	key    func() string
	logger zerolog.Logger
	wake   chan struct{}

	mu  sync.RWMutex
	cur *Release
}

// New builds an Updater caching APKs in dir (created on demand).
func New(dir string, store Store, fetch Fetcher, key func() string, logger zerolog.Logger) *Updater {
	return &Updater{dir: dir, store: store, fetch: fetch, key: key, wake: make(chan struct{}, 1),
		logger: logger.With().Str("module", "app-update").Logger()}
}

// Load restores the cached release from the DB (ignored if its file is gone).
func (u *Updater) Load(ctx context.Context) {
	raw, err := u.store.GetAppRelease(ctx)
	if err != nil || raw == "" {
		return
	}
	var r Release
	if err := json.Unmarshal([]byte(raw), &r); err != nil || r.VersionCode <= 0 {
		return
	}
	if st, err := os.Stat(u.Path(r)); err != nil || st.Size() != r.Size {
		u.logger.Warn().Ctx(ctx).Int64("version_code", r.VersionCode).Msg("cached APK missing; will re-download")
		return
	}
	u.mu.Lock()
	u.cur = &r
	u.mu.Unlock()
}

// Current is the cached release (nil when none).
func (u *Updater) Current() *Release {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.cur == nil {
		return nil
	}
	r := *u.cur
	return &r
}

// Path is where a release's APK lives.
func (u *Updater) Path(r Release) string {
	return filepath.Join(u.dir, fmt.Sprintf("%s-%d.apk", App, r.VersionCode))
}

// Wake asks for a check now (e.g. after the SSE stream reconnects).
func (u *Updater) Wake() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

// Run checks at start, on Wake and every `every` until ctx ends.
func (u *Updater) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := u.Check(ctx); err != nil {
			u.logger.Warn().Ctx(ctx).Err(err).Msg("app update check failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-u.wake:
		}
	}
}

// Check fetches main's newest release and caches it if it is newer.
func (u *Updater) Check(ctx context.Context) error {
	key := u.key()
	if key == "" {
		return nil // not provisioned yet
	}
	q := "app=" + App
	status, body, err := u.fetch.Get(ctx, key, "/store/v1/agent/app/latest", q)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusNotFound:
		return nil // no release yet (404: an older main without the route)
	case http.StatusOK:
	default:
		return fmt.Errorf("latest: HTTP %d", status)
	}
	var rel Release
	if err := json.Unmarshal(body, &rel); err != nil || rel.VersionCode <= 0 {
		return fmt.Errorf("latest: bad body")
	}
	if cur := u.Current(); cur != nil && rel.VersionCode <= cur.VersionCode {
		return nil
	}

	status, apk, err := u.fetch.Download(ctx, key, "/store/v1/agent/app/releases/"+strconv.FormatInt(rel.VersionCode, 10)+"/apk", q)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", status)
	}
	sum := sha256.Sum256(apk)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, rel.SHA256) || int64(len(apk)) != rel.Size {
		return fmt.Errorf("download: checksum/size mismatch")
	}
	if err := os.MkdirAll(u.dir, 0o755); err != nil {
		return err
	}
	tmp := u.Path(rel) + ".tmp"
	if err := os.WriteFile(tmp, apk, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, u.Path(rel)); err != nil {
		return err
	}
	raw, _ := json.Marshal(rel)
	if err := u.store.SetAppRelease(ctx, string(raw)); err != nil {
		return err
	}
	prev := u.Current()
	u.mu.Lock()
	u.cur = &rel
	u.mu.Unlock()
	u.prune(rel, prev)
	u.logger.Info().Ctx(ctx).Int64("version_code", rel.VersionCode).Str("version_name", rel.VersionName).Msg("New Quibase release cached for tablets")
	return nil
}

// prune keeps the current APK and the one it replaced.
func (u *Updater) prune(cur Release, prev *Release) {
	keep := map[string]bool{u.Path(cur): true}
	if prev != nil {
		keep[u.Path(*prev)] = true
	}
	matches, _ := filepath.Glob(filepath.Join(u.dir, App+"-*.apk"))
	for _, m := range matches {
		if !keep[m] {
			_ = os.Remove(m)
		}
	}
}
