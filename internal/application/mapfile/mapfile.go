// Package mapfile keeps the store's offline map (a PMTiles file) on the Pi so
// the tablets draw delivery maps without the internet. main publishes the
// file's URL + sha256 + version in store_config (location.map); when the
// version changes (or the file is missing at startup) the Pi downloads it once
// next to the SQLite file (/var/lib/quicc-agent/map.pmtiles on a Pi), checks
// the sha256, and swaps it in with a temp file + rename. One file is kept.
// GET /api/map.pmtiles serves it with Range support.
package mapfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/storeconfig"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// FileName is the map file's name in the agent's data directory.
const FileName = "map.pmtiles"

// maxBytes caps a download (a 10-mile store map is a few MB).
const maxBytes = 512 << 20

// Info is the downloaded map (persisted in settings.map_file as JSON).
type Info struct {
	Version string    `json:"version"`
	SHA256  string    `json:"sha256"`
	URL     string    `json:"url"`
	BBox    []float64 `json:"bbox"`
}

// Store persists Info (settings.map_file; "" = none).
type Store interface {
	GetMapFile(ctx context.Context) (string, error)
	SetMapFile(ctx context.Context, raw string) error
}

// Downloader follows store_config's map and keeps the file current.
type Downloader struct {
	path   string
	store  Store
	client *http.Client
	logger zerolog.Logger
	wake   chan struct{}
	retry  time.Duration

	mu      sync.RWMutex
	cur     *Info
	modTime time.Time
	wantSet bool
	want    *storeconfig.MapFile
	// failed remembers a version+sha whose download didn't verify, so a bad
	// publish is retried hourly instead of on every check.
	failed   string
	failedAt time.Time
	onChange func()
}

// OnChange registers a hook run after the file was replaced or removed (the
// tablets refetch /api/floor).
func (d *Downloader) OnChange(fn func()) { d.onChange = fn }

func (d *Downloader) changed() {
	if d.onChange != nil {
		d.onChange()
	}
}

// New builds a Downloader for the file at path.
func New(path string, store Store, logger zerolog.Logger) *Downloader {
	return &Downloader{path: path, store: store, wake: make(chan struct{}, 1), retry: 5 * time.Minute,
		client: &http.Client{Timeout: 10 * time.Minute, Transport: otelhttp.NewTransport(http.DefaultTransport)},
		logger: logger.With().Str("module", "map-file").Logger()}
}

// Path is where the map file lives.
func (d *Downloader) Path() string { return d.path }

// Load restores the downloaded map from the DB if the file is still there and
// matches its sha256 (a damaged file is dropped and downloaded again).
func (d *Downloader) Load(ctx context.Context) {
	raw, err := d.store.GetMapFile(ctx)
	if err != nil || raw == "" {
		return
	}
	var info Info
	if err := json.Unmarshal([]byte(raw), &info); err != nil || info.Version == "" {
		return
	}
	sum, st, err := hashFile(d.path)
	if err != nil || !strings.EqualFold(sum, info.SHA256) {
		d.logger.Warn().Ctx(ctx).Err(err).Str("version", info.Version).Msg("map file missing or damaged; will download again")
		_ = os.Remove(d.path)
		return
	}
	d.mu.Lock()
	d.cur, d.modTime = &info, st.ModTime()
	d.mu.Unlock()
}

// Current is the downloaded map (nil when none) and the file's time.
func (d *Downloader) Current() (*Info, time.Time) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.cur == nil {
		return nil, time.Time{}
	}
	c := *d.cur
	return &c, d.modTime
}

// Want sets the map store_config asks for (nil = none published) and wakes
// the loop. Until the first Want nothing is downloaded or removed.
func (d *Downloader) Want(m *storeconfig.MapFile) {
	d.mu.Lock()
	d.wantSet = true
	if m != nil {
		c := *m
		d.want = &c
	} else {
		d.want = nil
	}
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run checks on Want and retries every few minutes until ctx ends.
func (d *Downloader) Run(ctx context.Context) {
	t := time.NewTicker(d.retry)
	defer t.Stop()
	for {
		if err := d.Check(ctx); err != nil && ctx.Err() == nil {
			d.logger.Warn().Ctx(ctx).Err(err).Msg("map download failed; will retry")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.wake:
		}
	}
}

// Check makes the file match what store_config wants.
func (d *Downloader) Check(ctx context.Context) error {
	d.mu.RLock()
	set, want, cur := d.wantSet, d.want, d.cur
	failed, failedAt := d.failed, d.failedAt
	d.mu.RUnlock()
	if !set {
		return nil
	}
	if want == nil || want.URL == "" || want.Version == "" {
		if cur == nil {
			return nil
		}
		_ = os.Remove(d.path)
		d.mu.Lock()
		d.cur = nil
		d.mu.Unlock()
		d.logger.Info().Ctx(ctx).Str("version", cur.Version).Msg("store map unpublished; file removed")
		defer d.changed()
		return d.store.SetMapFile(ctx, "")
	}
	if cur != nil && cur.Version == want.Version && strings.EqualFold(cur.SHA256, want.SHA256) {
		if cur.URL != want.URL || !sameBBox(cur.BBox, want.BBox) { // metadata only
			return d.save(ctx, Info{Version: cur.Version, SHA256: cur.SHA256, URL: want.URL, BBox: want.BBox}, false)
		}
		return nil
	}
	key := want.Version + "|" + strings.ToLower(want.SHA256)
	if key == failed && time.Since(failedAt) < time.Hour {
		return nil
	}
	if err := d.download(ctx, *want); err != nil {
		d.mu.Lock()
		d.failed, d.failedAt = key, time.Now()
		d.mu.Unlock()
		return err
	}
	return nil
}

// ErrChecksum: the downloaded file isn't the published one.
var ErrChecksum = errors.New("map file sha256 mismatch")

func (d *Downloader) download(ctx context.Context, m storeconfig.MapFile) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("map download: HTTP %d", resp.StatusCode)
	}
	tmp := d.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxBytes {
		err = fmt.Errorf("map download: larger than %d bytes", maxBytes)
	}
	if err == nil && !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), m.SHA256) {
		err = ErrChecksum
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, d.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	d.logger.Info().Ctx(ctx).Str("version", m.Version).Int64("bytes", n).Msg("Store map downloaded")
	defer d.changed()
	return d.save(ctx, Info{Version: m.Version, SHA256: strings.ToLower(m.SHA256), URL: m.URL, BBox: m.BBox}, true)
}

func (d *Downloader) save(ctx context.Context, info Info, newFile bool) error {
	raw, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := d.store.SetMapFile(ctx, string(raw)); err != nil {
		return err
	}
	d.mu.Lock()
	d.cur = &info
	if newFile || d.modTime.IsZero() {
		if st, err := os.Stat(d.path); err == nil {
			d.modTime = st.ModTime()
		}
	}
	d.mu.Unlock()
	return nil
}

func hashFile(path string) (string, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", nil, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(h.Sum(nil)), st, nil
}

func sameBBox(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
