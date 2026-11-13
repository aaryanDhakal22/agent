// Package appupdate keeps the newest APK of an app cached on the agent so
// paired devices can update over the LAN: Quibase (the counter tablet; GET
// /api/app/latest + /api/app/apk) and the counter terminal (GET
// /api/terminal/app/latest + /api/terminal/app/apk, contract §2.10w). The
// console uploads a release to main (per app); the agent polls main (on
// every SSE connect + every few minutes), downloads a newer APK, verifies
// its sha256 and keeps it next to the SQLite file. One Updater per app.
package appupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// App is the tablet app (the Updater New builds); AppTerminal the counter
// terminal's (NewFor).
const (
	App         = "quibase"
	AppTerminal = "terminal"
)

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
	// DownloadTo streams the APK into w (≤ limit bytes; R-5: never whole in
	// memory).
	DownloadTo(ctx context.Context, key, path, rawQuery string, w io.Writer, limit int64) (int, int64, error)
}

type Updater struct {
	app    string
	dir    string
	store  Store
	fetch  Fetcher
	key    func() string
	logger zerolog.Logger
	wake   chan struct{}

	mu  sync.RWMutex
	cur *Release
	// checkMu: one Check at a time (the loop's and an "Update now" from main
	// share the temp file).
	checkMu sync.Mutex
	// onCached runs (in the checking goroutine) after a newer release is
	// cached.
	onCached func(Release)
}

// New builds the Quibase Updater caching APKs in dir (created on demand).
func New(dir string, store Store, fetch Fetcher, key func() string, logger zerolog.Logger) *Updater {
	return NewFor(App, dir, store, fetch, key, logger)
}

// NewFor builds an Updater for one app (main's ?app=): its files are
// <app>-<version code>.apk in dir, its metadata in store.
func NewFor(app, dir string, store Store, fetch Fetcher, key func() string, logger zerolog.Logger) *Updater {
	return &Updater{app: app, dir: dir, store: store, fetch: fetch, key: key, wake: make(chan struct{}, 1),
		logger: logger.With().Str("module", "app-update").Str("app", app).Logger()}
}

// App is the app this Updater caches.
func (u *Updater) App() string { return u.app }

// OnCached registers a hook run after a newer release is cached.
func (u *Updater) OnCached(fn func(Release)) { u.onCached = fn }

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
	return filepath.Join(u.dir, fmt.Sprintf("%s-%d.apk", u.app, r.VersionCode))
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

// MaxAPKBytes caps a cached app release (a release build with Square's SDK
// is 100-170 MB).
const MaxAPKBytes = 512 << 20

// validSHA256: 64 hex characters.
func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// Check fetches main's newest release and caches it if it is newer.
func (u *Updater) Check(ctx context.Context) error {
	u.checkMu.Lock()
	defer u.checkMu.Unlock()
	key := u.key()
	if key == "" {
		return nil // not provisioned yet
	}
	q := "app=" + u.app
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
	// The security round (M9): a release is taken only with its sha256 (64
	// hex: an empty one used to compare equal to nothing checked on the
	// tablets) and a sane size; the download below stops at that size.
	if !validSHA256(rel.SHA256) {
		return fmt.Errorf("latest: release %d has no sha256", rel.VersionCode)
	}
	if rel.Size <= 0 || rel.Size > MaxAPKBytes {
		return fmt.Errorf("latest: release %d has size %d (limit %d)", rel.VersionCode, rel.Size, int64(MaxAPKBytes))
	}
	if cur := u.Current(); cur != nil && rel.VersionCode <= cur.VersionCode {
		return nil
	}

	// Streamed to the temp file and hashed on the way (R-5: an APK is tens of
	// MB; the Pi used to hold it whole in memory).
	if err := os.MkdirAll(u.dir, 0o755); err != nil {
		return err
	}
	tmp := u.Path(rel) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	h := sha256.New()
	status, n, err := u.fetch.DownloadTo(ctx, key, "/store/v1/agent/app/releases/"+strconv.FormatInt(rel.VersionCode, 10)+"/apk", q,
		io.MultiWriter(f, h), rel.Size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("download: HTTP %d", status)
	}
	if err == nil {
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, rel.SHA256) || n != rel.Size {
			err = fmt.Errorf("download: checksum/size mismatch")
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
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
	u.logger.Info().Ctx(ctx).Int64("version_code", rel.VersionCode).Str("version_name", rel.VersionName).Msg("New app release cached for the store's devices")
	if u.onCached != nil {
		u.onCached(rel)
	}
	return nil
}

// prune keeps the current APK and the one it replaced.
func (u *Updater) prune(cur Release, prev *Release) {
	keep := map[string]bool{u.Path(cur): true}
	if prev != nil {
		keep[u.Path(*prev)] = true
	}
	matches, _ := filepath.Glob(filepath.Join(u.dir, u.app+"-*.apk"))
	for _, m := range matches {
		if !keep[m] {
			_ = os.Remove(m)
		}
	}
}
