// Package receiptlogo keeps the store's receipt logo on the Pi (contract
// §2.1j, 2026-11-12): store_config names it (receipt_logo {id, etag,
// content_type}, absent = none); the bytes come from main's GET
// /store/v1/agent/receipt-logo (agent key, If-None-Match → 304), are kept next
// to the DB (receipt-logo.bin + .etag) so a restart or an offline Pi still
// prints it, decoded and handed to the receipt package.
package receiptlogo

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/infra/printer/receipt"

	"github.com/rs/zerolog"
)

// Fetcher GETs main's /store/v1/agent/receipt-logo with the store key and
// If-None-Match (empty = none): status, bytes, the answer's ETag.
type Fetcher func(ctx context.Context, ifNoneMatch string) (int, []byte, string, error)

// Keeper owns the logo file.
type Keeper struct {
	dir    string
	fetch  Fetcher
	logger zerolog.Logger

	mu      sync.Mutex
	etag    string // the installed logo's ETag ("" = none)
	pending string // the ETag being downloaded now
}

// New keeps the logo in dir (the DB's directory).
func New(dir string, fetch Fetcher, logger zerolog.Logger) *Keeper {
	return &Keeper{dir: dir, fetch: fetch, logger: logger.With().Str("module", "receipt-logo").Logger()}
}

func (k *Keeper) binPath() string  { return k.dir + "/receipt-logo.bin" }
func (k *Keeper) etagPath() string { return k.dir + "/receipt-logo.etag" }

// Load installs the logo kept on disk (at start, before main answers).
func (k *Keeper) Load() {
	b, err := os.ReadFile(k.binPath())
	if err != nil {
		return
	}
	et, _ := os.ReadFile(k.etagPath())
	img, err := receipt.DecodeReceiptLogo(b)
	if err != nil {
		k.logger.Warn().Err(err).Msg("Receipt logo on disk can't be read: left out")
		return
	}
	k.mu.Lock()
	k.etag = strings.TrimSpace(string(et))
	k.mu.Unlock()
	receipt.SetReceiptLogo(img, k.etag)
}

// Want is store_config's receipt_logo: none clears it, a new ETag downloads it
// (in the background; a failure keeps the one there and tries again at the
// next store_config).
func (k *Keeper) Want(ctx context.Context, ref *storeconfig.ReceiptLogo) {
	if ref == nil || strings.TrimSpace(ref.ETag) == "" {
		k.clear()
		return
	}
	if k.fetch == nil {
		return
	}
	k.mu.Lock()
	if k.etag == ref.ETag || k.pending == ref.ETag {
		k.mu.Unlock()
		return
	}
	k.pending = ref.ETag
	k.mu.Unlock()
	go func() {
		k.download(context.WithoutCancel(ctx), ref.ETag)
		k.mu.Lock()
		k.pending = ""
		k.mu.Unlock()
	}()
}

func (k *Keeper) clear() {
	k.mu.Lock()
	had := k.etag != ""
	k.etag = ""
	k.mu.Unlock()
	if had {
		receipt.SetReceiptLogo(nil, "")
		_ = os.Remove(k.binPath())
		_ = os.Remove(k.etagPath())
		k.logger.Info().Msg("Receipt logo removed (the store has none)")
	}
}

func (k *Keeper) download(ctx context.Context, etag string) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	k.mu.Lock()
	cur := k.etag
	k.mu.Unlock()
	status, body, respETag, err := k.fetch(ctx, cur)
	switch {
	case err != nil:
		k.logger.Warn().Err(err).Msg("Receipt logo not downloaded (main unreachable): kept the one there")
		return
	case status == http.StatusNotModified:
		return
	case status == http.StatusNotFound:
		k.clear()
		return
	case status != http.StatusOK:
		k.logger.Warn().Int("status", status).Msg("Receipt logo not downloaded")
		return
	}
	img, err := receipt.DecodeReceiptLogo(body)
	if err != nil {
		k.logger.Warn().Err(err).Msg("Receipt logo can't be read (not PNG / JPEG / WebP): left out")
		return
	}
	if e := strings.TrimSpace(respETag); e != "" {
		etag = e
	}
	tmp := k.binPath() + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err == nil {
		_ = os.Rename(tmp, k.binPath())
		_ = os.WriteFile(k.etagPath(), []byte(etag), 0o644)
	}
	k.mu.Lock()
	k.etag = etag
	k.mu.Unlock()
	receipt.SetReceiptLogo(img, etag)
	k.logger.Info().Int("bytes", len(body)).Msg("Receipt logo installed")
}
