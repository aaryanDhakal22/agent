package order

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
)

// 2.3.2: the callout PREVIEW "on this tablet" - hub checks, the feed's
// callout.render, pending -> ready / failed, the 20 s timeout, the TTL and
// the entry cap, the upload checks.

func testWAV(n int) []byte {
	b := make([]byte, 44+n)
	copy(b, "RIFF")
	copy(b[8:], "WAVE")
	return b
}

func hubStatus(t *testing.T, svc *Service, callout string) {
	t.Helper()
	st := order.IntegrationStatus{OK: true, Problems: []order.Problem{}}
	if callout != "" {
		st.Callout = json.RawMessage(callout)
	}
	if err := svc.SetIntegrationStatus(context.Background(), st); err != nil {
		t.Fatal(err)
	}
}

func unavailable(t *testing.T, err error) string {
	t.Helper()
	var un *PreviewUnavailable
	if !errors.As(err, &un) {
		t.Fatalf("want PreviewUnavailable, got %v", err)
	}
	return un.Code
}

func TestCalloutPreviewNeedsAHubThatCanRender(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	if _, err := svc.CalloutPreview(ctx, 0, ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("number 0: %v", err)
	}
	if _, err := svc.CalloutPreview(ctx, 122, "bad voice"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad voice: %v", err)
	}
	if _, err := svc.CalloutPreview(ctx, 122, ""); unavailable(t, err) != PreviewHubNotConnected {
		t.Fatal("no hub")
	}
	hubStatus(t, svc, "") // a hub without the callout
	if _, err := svc.CalloutPreview(ctx, 122, ""); unavailable(t, err) != PreviewHubUpdateNeeded {
		t.Fatal("hub without callout")
	}
	hubStatus(t, svc, `{"installed":true,"voices":[]}`) // hub 0.2.0/0.2.1: no preview
	if _, err := svc.CalloutPreview(ctx, 122, ""); unavailable(t, err) != PreviewHubUpdateNeeded {
		t.Fatal("older hub")
	}
	hubStatus(t, svc, `{"installed":false,"preview":true}`)
	if _, err := svc.CalloutPreview(ctx, 122, ""); unavailable(t, err) != PreviewNotInstalled {
		t.Fatal("not installed")
	}
	if v := svc.Callout(time.Now()); !v.Preview {
		t.Fatal("GET /api/callout must say preview")
	}

	hubStatus(t, svc, `{"installed":true,"preview":true}`)
	res, err := svc.CalloutPreview(ctx, 122, "kokoro:bf_emma")
	if err != nil || res.Status != "pending" || res.ID == "" {
		t.Fatalf("preview = %+v %v", res, err)
	}
	page, err := svc.Feed(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got *order.CalloutRenderData
	for _, ev := range page.Events {
		if ev.Type == order.FeedCalloutRender {
			got = &order.CalloutRenderData{}
			_ = json.Unmarshal(ev.Data, got)
		}
	}
	if got == nil || got.ID != res.ID || got.Number != 122 || got.Voice != "kokoro:bf_emma" {
		t.Fatalf("feed callout.render = %+v (%d events)", got, len(page.Events))
	}
}

func TestCalloutPreviewPendingReadyFailedTimeout(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	hubStatus(t, svc, `{"installed":true,"preview":true}`)
	now := time.Date(2026, 10, 26, 19, 0, 0, 0, time.UTC)
	svc.preview.now = func() time.Time { return now }

	a, _ := svc.CalloutPreview(ctx, 122, "")
	if wav, err := svc.CalloutPreviewAudio(ctx, a.ID, 0); wav != nil || err != nil {
		t.Fatalf("pending = %v %v", wav, err)
	}
	// A waiting fetch returns as soon as the hub uploads.
	done := make(chan []byte, 1)
	go func() {
		wav, _ := svc.CalloutPreviewAudio(ctx, a.ID, 5*time.Second)
		done <- wav
	}()
	time.Sleep(20 * time.Millisecond)
	if err := svc.PutCalloutAudio(ctx, a.ID, testWAV(1000)); err != nil {
		t.Fatal(err)
	}
	select {
	case wav := <-done:
		if len(wav) != 1044 {
			t.Fatalf("wav = %d bytes", len(wav))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting fetch did not wake up")
	}

	// Upload checks: unknown id, not a WAV, too big.
	if err := svc.PutCalloutAudio(ctx, "nope", testWAV(10)); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	b, _ := svc.CalloutPreview(ctx, 7, "")
	if err := svc.PutCalloutAudio(ctx, b.ID, []byte("not a wav file at all, just some text here...........")); !errors.Is(err, ErrPreviewNotWAV) {
		t.Fatalf("not wav: %v", err)
	}
	if err := svc.PutCalloutAudio(ctx, b.ID, testWAV(MaxPreviewAudio)); !errors.Is(err, ErrPreviewTooLarge) {
		t.Fatalf("too large: %v", err)
	}

	// The hub could not render it: 503 with its reason.
	if err := svc.FailCalloutPreview(ctx, b.ID, "weird", "renderer crashed"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CalloutPreviewAudio(ctx, b.ID, 0); unavailable(t, err) != PreviewRenderFailed || err.Error() != "renderer crashed" {
		t.Fatalf("failed: %v", err)
	}

	// Nothing from the hub for 20 s: timeout (a wait doesn't outlast it).
	c, _ := svc.CalloutPreview(ctx, 8, "")
	now = now.Add(PreviewTimeout)
	start := time.Now()
	if _, err := svc.CalloutPreviewAudio(ctx, c.ID, 5*time.Second); !errors.Is(err, ErrPreviewTimeout) || time.Since(start) > time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
	// A late upload still counts while the entry lives.
	if err := svc.PutCalloutAudio(ctx, c.ID, testWAV(10)); err != nil {
		t.Fatal(err)
	}
	if wav, err := svc.CalloutPreviewAudio(ctx, c.ID, 0); err != nil || wav == nil {
		t.Fatalf("late upload: %v", err)
	}

	// TTL: forgotten 2 min after asking.
	now = now.Add(PreviewTTL)
	if _, err := svc.CalloutPreviewAudio(ctx, a.ID, 0); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("expired: %v", err)
	}
	if err := svc.PutCalloutAudio(ctx, c.ID, testWAV(10)); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("upload after TTL: %v", err)
	}
}

func TestCalloutPreviewKeepsAFewEntries(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	hubStatus(t, svc, `{"installed":true,"preview":true}`)
	var ids []string
	for i := 0; i < PreviewKeep+3; i++ {
		r, err := svc.CalloutPreview(ctx, 100+i, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	for i, id := range ids {
		_, err := svc.CalloutPreviewAudio(ctx, id, 0)
		if gone := errors.Is(err, ErrPreviewNotFound); gone != (i < 3) {
			t.Fatalf("entry %d: gone=%v err=%v", i, gone, err)
		}
	}
}
