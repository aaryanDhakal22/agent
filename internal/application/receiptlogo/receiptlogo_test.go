package receiptlogo

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/infra/printer/receipt"

	"github.com/rs/zerolog"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 40, 20))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The keeper downloads on a new ETag only, keeps the file for a restart and
// clears it when store_config has no receipt_logo.
func TestKeeper(t *testing.T) {
	dir := t.TempDir()
	img := pngBytes(t)
	var calls atomic.Int32
	var sentINM atomic.Value
	fetch := func(_ context.Context, inm string) (int, []byte, string, error) {
		calls.Add(1)
		sentINM.Store(inm)
		if inm == `"l1"` {
			return 304, nil, `"l1"`, nil
		}
		return 200, img, `"l1"`, nil
	}
	t.Cleanup(func() { receipt.SetReceiptLogo(nil, "") })
	k := New(dir, fetch, zerolog.Nop())
	ctx := context.Background()

	k.Want(ctx, &storeconfig.ReceiptLogo{ID: "a", ETag: `"l1"`, ContentType: "image/png"})
	waitFor(t, "the logo", func() bool { return receipt.ReceiptLogoETag() == `"l1"` })
	if _, err := os.Stat(dir + "/receipt-logo.bin"); err != nil {
		t.Fatalf("kept next to the DB: %v", err)
	}
	k.Want(ctx, &storeconfig.ReceiptLogo{ID: "a", ETag: `"l1"`})
	if calls.Load() != 1 {
		t.Fatalf("same ETag downloads again: %d", calls.Load())
	}

	// A restart: Load installs the file before main answers.
	receipt.SetReceiptLogo(nil, "")
	k2 := New(dir, fetch, zerolog.Nop())
	k2.Load()
	if receipt.ReceiptLogoETag() != `"l1"` {
		t.Fatalf("Load: %q", receipt.ReceiptLogoETag())
	}
	// A new ETag asks with the old one as If-None-Match.
	k2.Want(ctx, &storeconfig.ReceiptLogo{ID: "b", ETag: `"l2"`})
	waitFor(t, "the second fetch", func() bool { return calls.Load() == 2 })
	if v, _ := sentINM.Load().(string); v != `"l1"` {
		t.Errorf("If-None-Match %q", v)
	}

	// No receipt_logo: cleared, file gone.
	k2.Want(ctx, nil)
	if receipt.ReceiptLogoETag() != "" {
		t.Fatalf("cleared: %q", receipt.ReceiptLogoETag())
	}
	if _, err := os.Stat(dir + "/receipt-logo.bin"); !os.IsNotExist(err) {
		t.Fatalf("file removed: %v", err)
	}
}
