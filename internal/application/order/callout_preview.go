package order

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// The callout PREVIEW "on this tablet" (2.3.2, contract §2.9.10): the tablet
// asks (POST /api/callout/preview) -> the Pi writes the feed's callout.render
// -> the store hub renders the words (or takes them from its WAV cache) and
// uploads the WAV (PUT /api/integration/callout-audio/{id}) -> the tablet
// fetches it (GET /api/callout/preview/{id}) and plays it on its own speaker.
// Nothing plays in the store. The Pi never calls the hub; the audio is kept
// in memory only (a few entries, a short TTL) - no customer data in it.

const (
	// PreviewTimeout: no audio this long after asking -> 504 timeout.
	PreviewTimeout = 20 * time.Second
	// PreviewTTL: an entry (pending, ready or failed) is forgotten this long
	// after it was asked for.
	PreviewTTL = 2 * time.Minute
	// PreviewKeep: at most this many entries; the oldest goes first.
	PreviewKeep = 8
	// MaxPreviewAudio caps an uploaded WAV (a callout is ~130 KB).
	MaxPreviewAudio = 2 << 20
	// MaxPreviewWait caps GET ?wait= (seconds).
	MaxPreviewWait = 10
)

// Preview failure codes (503 code).
const (
	PreviewHubNotConnected = "hub_not_connected"
	PreviewHubUpdateNeeded = "hub_update_needed"
	PreviewNotInstalled    = "not_installed"
	PreviewRenderFailed    = "render_failed"
)

var (
	// ErrPreviewNotFound: no such preview (never asked, or older than PreviewTTL).
	ErrPreviewNotFound = errors.New("no such preview (unknown or expired)")
	// ErrPreviewTimeout: the hub sent nothing within PreviewTimeout.
	ErrPreviewTimeout = errors.New("the store hub didn't send the voice in time")
	// ErrPreviewNotWAV: the upload isn't a RIFF/WAVE file.
	ErrPreviewNotWAV = errors.New("the audio must be a WAV file")
	// ErrPreviewTooLarge: the upload is over MaxPreviewAudio.
	ErrPreviewTooLarge = fmt.Errorf("the audio is over %d bytes", MaxPreviewAudio)
)

// PreviewUnavailable is a 503: the preview can't be (or wasn't) made.
type PreviewUnavailable struct {
	Code    string
	Message string
}

func (e *PreviewUnavailable) Error() string { return e.Message }

// CalloutPreviewResult is POST /api/callout/preview's answer.
type CalloutPreviewResult struct {
	ID     string `json:"id"`
	Status string `json:"status"` // pending
}

type previewEntry struct {
	id       string
	at       time.Time
	wav      []byte
	failCode string
	failMsg  string
	done     chan struct{} // closed on the hub's first answer
	answered bool
}

// previewState is the in-memory preview store.
type previewState struct {
	mu   sync.Mutex
	m    map[string]*previewEntry
	ids  []string // oldest first
	now  func() time.Time
}

func (p *previewState) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// prune drops expired entries and keeps at most PreviewKeep-1 (room for one
// more). Caller holds mu.
func (p *previewState) prune(now time.Time, room int) {
	if p.m == nil {
		p.m = map[string]*previewEntry{}
	}
	keep := p.ids[:0]
	for _, id := range p.ids {
		if e := p.m[id]; e != nil && now.Sub(e.at) < PreviewTTL {
			keep = append(keep, id)
		} else {
			delete(p.m, id)
		}
	}
	p.ids = keep
	for len(p.ids) > PreviewKeep-room {
		delete(p.m, p.ids[0])
		p.ids = p.ids[1:]
	}
}

func (p *previewState) add(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	p.prune(now, 1)
	p.m[id] = &previewEntry{id: id, at: now, done: make(chan struct{})}
	p.ids = append(p.ids, id)
}

func (p *previewState) get(id string) (*previewEntry, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	p.prune(now, 0)
	return p.m[id], now
}

// answer stores the hub's answer (audio or a failure) for id.
func (p *previewState) answer(id string, wav []byte, code, msg string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(p.clock(), 0)
	e := p.m[id]
	if e == nil {
		return ErrPreviewNotFound
	}
	e.wav, e.failCode, e.failMsg = wav, code, msg
	if !e.answered {
		e.answered = true
		close(e.done)
	}
	return nil
}

// hubPreviewReport is what the preview needs of the hub's callout report.
type hubPreviewReport struct {
	Preview   bool  `json:"preview"`
	Installed *bool `json:"installed"`
}

// CalloutPreview asks the store hub to render a callout for this tablet:
// feed callout.render {id, voice, number}. 503 (PreviewUnavailable) when
// the hub is not connected, has no tablet preview (older hub) or no voice
// installed.
func (s *Service) CalloutPreview(ctx context.Context, number int, voice string) (*CalloutPreviewResult, error) {
	ctx, span := s.span(ctx, "callout.preview", attribute.Int("number", number))
	defer span.End()
	if number < 1 || number > order.MaxCalloutNumber {
		return nil, spanErr(span, fmt.Errorf("%w: number must be 1-%d", ErrBadRequest, order.MaxCalloutNumber))
	}
	voice = strings.TrimSpace(voice)
	if err := order.ValidateVoice(voice); err != nil {
		return nil, spanErr(span, fmt.Errorf("%w: %w", ErrBadRequest, err))
	}
	v := s.IntegrationView(time.Now())
	if !v.Connected {
		return nil, spanErr(span, &PreviewUnavailable{PreviewHubNotConnected, "The store hub is not connected, so it can't make the voice."})
	}
	var rep hubPreviewReport
	if len(v.Callout) > 0 {
		_ = json.Unmarshal(v.Callout, &rep)
	}
	switch {
	case len(v.Callout) == 0 || !rep.Preview:
		return nil, spanErr(span, &PreviewUnavailable{PreviewHubUpdateNeeded, "The store hub needs an update to play a preview on the tablet."})
	case rep.Installed != nil && !*rep.Installed:
		return nil, spanErr(span, &PreviewUnavailable{PreviewNotInstalled, "The voice is not installed on the store computer yet."})
	}
	id := uuid.NewString()
	s.preview.add(id)
	if err := s.repo.InsertCalloutRender(ctx, order.CalloutRenderData{ID: id, Voice: voice, Number: number}); err != nil {
		return nil, spanErr(span, err)
	}
	s.feedNotify()
	s.devEvent(devlog.Info, devlog.KindHub, "Callout preview #%d asked of the store hub (for the tablet)", number)
	return &CalloutPreviewResult{ID: id, Status: "pending"}, nil
}

// CalloutPreviewAudio is the tablet's fetch: the WAV once the hub sent it,
// nil (pending) before, waiting up to wait for it. ErrPreviewNotFound,
// ErrPreviewTimeout (no audio PreviewTimeout after asking) or a
// *PreviewUnavailable the hub reported.
func (s *Service) CalloutPreviewAudio(ctx context.Context, id string, wait time.Duration) ([]byte, error) {
	e, now := s.preview.get(id)
	if e == nil {
		return nil, ErrPreviewNotFound
	}
	left := PreviewTimeout - now.Sub(e.at)
	if wait > 0 && left > 0 {
		t := time.NewTimer(min(wait, left))
		select {
		case <-e.done:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
		e, now = s.preview.get(id)
		if e == nil {
			return nil, ErrPreviewNotFound
		}
	}
	s.preview.mu.Lock()
	wav, code, msg, answered := e.wav, e.failCode, e.failMsg, e.answered
	s.preview.mu.Unlock()
	switch {
	case answered && code != "":
		return nil, &PreviewUnavailable{code, msg}
	case answered:
		return wav, nil
	case now.Sub(e.at) >= PreviewTimeout:
		return nil, ErrPreviewTimeout
	}
	return nil, nil
}

// IsWAV reports whether b looks like a RIFF/WAVE file.
func IsWAV(b []byte) bool {
	return len(b) > 44 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WAVE"))
}

// PutCalloutAudio stores the hub's rendered WAV for preview id.
func (s *Service) PutCalloutAudio(_ context.Context, id string, wav []byte) error {
	switch {
	case len(wav) > MaxPreviewAudio:
		return ErrPreviewTooLarge
	case !IsWAV(wav):
		return ErrPreviewNotWAV
	}
	if err := s.preview.answer(id, wav, "", ""); err != nil {
		return err
	}
	s.devEvent(devlog.Info, devlog.KindHub, "Callout preview ready for the tablet (%d KB)", len(wav)/1024)
	return nil
}

// FailCalloutPreview stores the hub's reason it could not render preview
// id (code not_installed | render_failed; anything else reads render_failed).
func (s *Service) FailCalloutPreview(_ context.Context, id, code, msg string) error {
	if code != PreviewNotInstalled {
		code = PreviewRenderFailed
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = "The store computer could not make the voice."
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	if err := s.preview.answer(id, nil, code, msg); err != nil {
		return err
	}
	s.devEvent(devlog.Warn, devlog.KindHub, "Callout preview failed on the store hub: %s", msg)
	return nil
}
