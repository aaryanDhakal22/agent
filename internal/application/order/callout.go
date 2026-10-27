package order

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// Finishing orders, terminal changes to POS checks and the store hub's order
// callout (00020; contract §2.9.8-2.9.10).
//
//   - A ready pickup / counter order is "finished" (picked up) when the POS
//     closes its check (terminal orders) or N minutes after ready (online
//     pickups, settings.finishing). It stays ready but no longer counts as
//     open, so a forced Pi update finds an idle moment mid-shift.
//   - A terminal change to a check already on the tablet: a kitchen line
//     added or removed rings the tablets until OK (SSE order.changed, the
//     order's status.pos_change); drinks update quietly. A ready order with
//     a kitchen line added goes back to cooking (a delivery leaves its
//     pool); one already out only alerts. Removed lines stay struck through.
//   - The callout: settings ride the feed's settings.changed; TEST CALLOUT /
//     PREVIEW write callout.test; the hub reports its voices in its status.

// ErrNoChange: an ack for an order with no unacknowledged change.
var ErrNoChange = errors.New("no change to acknowledge")

// afterPOSUpsert publishes what UpsertExternal's repo result says happened
// beyond a plain update.
func (s *Service) afterPOSUpsert(ctx context.Context, res order.ExternalResult) {
	so := res.Order
	if res.Reopened {
		s.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Msg("POS order back to cooking: a kitchen item was added at the terminal")
		s.devEvent(devlog.Info, devlog.KindOrder, "%s back to cooking (item added at the POS)", label(so))
		for _, x := range s.resequencePools(ctx, nil) {
			s.publishUpdated(x)
		}
		s.floorChanged()
	}
	if c := res.Change; c != nil && s.broker != nil {
		s.broker.Publish("order.changed", map[string]any{"order": so, "change": c})
	}
	if c := res.Change; c != nil {
		s.devEvent(devlog.Info, devlog.KindOrder, "%s", c.Text)
	}
	if res.Finished {
		s.devEvent(devlog.Info, devlog.KindOrder, "%s finished (closed at the POS)", label(so))
	}
	if len(res.VoidPrint) > 0 && s.IntegrationSettings().Tickets.POSFor(so.Order.Fulfillment.Type) {
		s.printVoid(ctx, so, res.VoidPrint)
	}
}

// printVoid prints a VOID ticket for lines voided at the terminal, on the
// stations they printed on (background; a dead printer only alerts).
func (s *Service) printVoid(ctx context.Context, so order.StoredOrder, items []order.LineItem) {
	snap := printable(so)
	snap.Items = append([]order.LineItem(nil), items...)
	snap.RemovedItems = nil
	s.background(ctx, func(ctx context.Context) {
		out, err := s.print(ctx, snap, printer.ModeVoid)
		if errors.Is(err, printer.ErrNoTickets) {
			return
		}
		s.alertPrintIssues(ctx, so, out, false)
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", so.Order.ID).Msg("POS void ticket not printed")
			return
		}
		s.printEvents(label(so)+" VOID (POS)", out)
	})
}

// AckPosChange is OK on the change alert: the change is marked acked_at
// (every tablet stops ringing for it via order.updated; the ticket keeps its
// CHANGED tag). changeID "" acks whatever is
// there; a newer change than the one OK'd keeps ringing (409 no_change).
func (s *Service) AckPosChange(ctx context.Context, id, changeID string) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.ack_change", attribute.String("order.id", id))
	defer span.End()
	if _, err := uuid.Parse(id); err != nil {
		return nil, spanErr(span, fmt.Errorf("%w: invalid order id", ErrBadRequest))
	}
	so, acked, err := s.repo.AckPosChange(ctx, id, strings.TrimSpace(changeID))
	if err != nil {
		return nil, spanErr(span, err)
	}
	if !acked {
		return so, spanErr(span, ErrNoChange)
	}
	s.publishUpdated(*so)
	s.devEvent(devlog.Info, devlog.KindOrder, "%s change acknowledged", label(*so))
	return so, nil
}

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
	if err := (order.CalloutSettings{Voice: voice}).Validate(); err != nil {
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
}

// Callout returns the callout settings and the hub's report.
func (s *Service) Callout(now time.Time) CalloutView {
	v := s.IntegrationView(now)
	var hub any
	if len(v.Callout) > 0 {
		hub = v.Callout
	}
	return CalloutView{Settings: s.IntegrationSettings().Callout, HubConnected: v.Connected, Hub: hub}
}
