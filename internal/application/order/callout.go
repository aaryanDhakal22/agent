package order

import (
	"context"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// Finishing orders and the store hub's order callout (00019; contract
// §2.9.9-2.9.10).
//
//   - A ready pickup / counter order is "finished" (picked up) when a
//     terminal hands it over (terminal orders) or N minutes after ready
//     (online pickups, settings.finishing). It stays ready but no longer
//     counts as open, so a forced Pi update finds an idle moment mid-shift.
//   - The callout: settings ride the feed's settings.changed; TEST CALLOUT /
//     PREVIEW write callout.test; the hub reports its voices in its status.

// FinishDue finishes online pickups ready for longer than the store's finish
// time (settings.finishing; 0 = never). Run on the escalation tick.
func (s *Service) FinishDue(ctx context.Context, now time.Time) (int, error) {
	mins := s.IntegrationSettings().Finishing.OnlinePickupMinutes
	if mins <= 0 {
		return 0, nil
	}
	list, err := s.repo.FinishDueOnlinePickups(ctx, now.Add(-time.Duration(mins)*time.Minute), now)
	if err != nil {
		return 0, err
	}
	for _, so := range list {
		s.publishUpdated(so)
		s.devEvent(devlog.Info, devlog.KindOrder, "%s finished (ready %d min)", label(so), mins)
	}
	return len(list), nil
}

// CalloutTestResult is POST /api/callout/test's answer.
type CalloutTestResult struct {
	ID           string `json:"id"`
	HubConnected bool   `json:"hub_connected"`
}

// CalloutTest asks the store hub to speak a callout now (TEST CALLOUT with a
// number, or a voice PREVIEW) through the feed's callout.test.
func (s *Service) CalloutTest(ctx context.Context, kind string, number int, voice string) (*CalloutTestResult, error) {
	ctx, span := s.span(ctx, "callout.test", attribute.String("kind", kind), attribute.Int("number", number))
	defer span.End()
	switch kind {
	case "":
		kind = order.CalloutTest
	case order.CalloutTest, order.CalloutPreview:
	default:
		return nil, spanErr(span, fmt.Errorf("%w: kind must be test or preview", ErrBadRequest))
	}
	if number < 1 || number > order.MaxCalloutNumber {
		return nil, spanErr(span, fmt.Errorf("%w: number must be 1-%d", ErrBadRequest, order.MaxCalloutNumber))
	}
	voice = strings.TrimSpace(voice)
	if err := order.ValidateVoice(voice); err != nil {
		return nil, spanErr(span, fmt.Errorf("%w: %w", ErrBadRequest, err))
	}
	t := order.CalloutTestData{ID: uuid.NewString(), Kind: kind, Number: number, Voice: voice}
	if err := s.repo.InsertCalloutTest(ctx, t); err != nil {
		return nil, spanErr(span, err)
	}
	s.feedNotify()
	s.devEvent(devlog.Info, devlog.KindHub, "Callout %s #%d asked of the store hub", kind, number)
	return &CalloutTestResult{ID: t.ID, HubConnected: s.IntegrationView(time.Now()).Connected}, nil
}

// CalloutView is GET /api/callout: the settings, and what the hub reports
// (voices, rendering, the last callout) - null until a hub with the callout
// reported.
type CalloutView struct {
	Settings     order.CalloutSettings `json:"settings"`
	HubConnected bool                  `json:"hub_connected"`
	Hub          any                   `json:"hub"`
	// Preview (2.3.2): this Pi relays a PREVIEW to the tablet's own speaker
	// (POST /api/callout/preview). An older Pi leaves it out.
	Preview bool `json:"preview"`
}

// Callout returns the callout settings and the hub's report.
func (s *Service) Callout(now time.Time) CalloutView {
	v := s.IntegrationView(now)
	var hub any
	if len(v.Callout) > 0 {
		hub = v.Callout
	}
	return CalloutView{Settings: s.IntegrationSettings().Callout, HubConnected: v.Connected, Hub: hub, Preview: true}
}
