package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// Website / channel orders on the floor (2.5.0, weborder stage 3): ADD TIP
// (main adds it to the card capture), CANCEL ORDER (main releases the card
// hold; the stations get a VOID slip) and Quibase's online-ordering controls
// - pause / resume the store's channels and sold out - proxied to main over
// the duplex channel (agent -> main RPCs answered with data).

// Agent -> main online-ordering RPCs (main's onlinectl).
const (
	MsgOnlineStatus = "online.status"
	MsgOnlinePause  = "online.pause"
	MsgOnlineResume = "online.resume"
	MsgMenuStock    = "menu.stock"
	MsgMenuStockSet = "menu.stock.set"
)

// ErrMainUnreachable: main didn't answer an online-ordering proxy call (503
// main_unreachable).
var ErrMainUnreachable = errors.New("can't reach the server - try again")

// onlineWait is how long an online-ordering proxy waits for main.
const onlineWait = 10 * time.Second

// pausePresets are the pause lengths Quibase offers (main checks them too).
var pausePresets = map[string]bool{"15m": true, "30m": true, "60m": true, "today": true}

// SetTip is ADD TIP: the whole tip added after the order (absolute, 0-50000
// cents) on a card channel order of the open batch whose card main hasn't
// charged yet. main hears it as order.tip.
func (s *Service) SetTip(ctx context.Context, id string, cents int64) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.set_tip", attribute.String("order.id", id), attribute.Int64("tip_cents", cents))
	defer span.End()
	if _, err := uuid.Parse(id); err != nil {
		return nil, spanErr(span, fmt.Errorf("%w: invalid order id", ErrBadRequest))
	}
	if cents < 0 || cents > order.MaxAddedTipCents {
		return nil, spanErr(span, fmt.Errorf("%w: tip_cents must be 0-%d", ErrBadRequest, order.MaxAddedTipCents))
	}
	so, err := s.repo.SetAddedTip(ctx, id, cents)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.floorChanged()
	s.publishUpdated(*so)
	s.devEvent(devlog.Info, devlog.KindOrder, "%s tip added on the tablet: %s", label(*so), cents2(cents))
	s.logger.Info().Ctx(ctx).Str("order_id", id).Int64("tip_cents", cents).Msg("Tip added after the order")
	return so, nil
}

// CancelOrder is CANCEL ORDER on a channel (website) order not delivered /
// picked up yet: cancelled with the reason (main cancels it and releases the
// card hold), off its pool or run, and a VOID slip on the stations its
// tickets went to when it was accepted. note (<= 140 characters) is only
// logged.
func (s *Service) CancelOrder(ctx context.Context, id, reason, note string) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.cancel_channel", attribute.String("order.id", id), attribute.String("reason", reason))
	defer span.End()
	if _, err := uuid.Parse(id); err != nil {
		return nil, spanErr(span, fmt.Errorf("%w: invalid order id", ErrBadRequest))
	}
	if !order.ValidCancelReason(reason) {
		return nil, spanErr(span, fmt.Errorf("%w: reason must be customer_asked, couldnt_deliver or other", ErrBadRequest))
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > order.MaxCancelNoteRunes {
		return nil, spanErr(span, fmt.Errorf("%w: note is at most %d characters", ErrBadRequest, order.MaxCancelNoteRunes))
	}
	so, prev, returned, err := s.repo.CancelChannelOrder(ctx, id, reason)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.floorChanged()
	s.publishUpdated(*so)
	if prev.Status.PoolDriverID != "" {
		s.publishAll(s.resequencePools(ctx, nil))
	}
	if prev.Status.PoolDriverID != "" || prev.Status.DriverID != "" || returned {
		s.publishDrivers(ctx)
	}
	if printedTickets(prev) {
		s.printCancelVoid(ctx, prev)
	}
	s.devEvent(devlog.Info, devlog.KindOrder, "%s cancelled on the floor (%s)", label(*so), reason)
	s.logger.Info().Ctx(ctx).Str("order_id", id).Str("reason", reason).Str("note", note).
		Str("was", string(prev.Status.State)).Bool("driver_returned", returned).Msg("Website order cancelled")
	return so, nil
}

// printedTickets: the order's station tickets printed (it was accepted, or
// a future order's kitchen tickets printed early).
func printedTickets(so order.StoredOrder) bool {
	switch so.Status.State {
	case order.StateAccepted, order.StateReady, order.StateOnDelivery:
		return true
	}
	return so.Status.EarlyKitchen
}

// printCancelVoid prints the VOID slip for a cancelled order on the stations
// its tickets went to (background; a dead printer only alerts).
func (s *Service) printCancelVoid(ctx context.Context, so order.StoredOrder) {
	snap := printable(so)
	snap.RemovedItems = nil
	s.background(ctx, func(ctx context.Context) {
		out, err := s.printJob(ctx, snap, printer.ModeVoid)
		if errors.Is(err, printer.ErrNoTickets) {
			return
		}
		s.alertPrintIssues(ctx, so, out, false)
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", so.Order.ID).Msg("Cancel VOID slip not printed")
			return
		}
		s.printEvents(label(so)+" VOID (cancelled)", out)
	})
}

func cents2(c int64) string { return fmt.Sprintf("$%d.%02d", c/100, c%100) }

// --- online-ordering controls (proxied to main) -----------------------------

// OnlineStatus is GET /api/online: main's online.status reply.
func (s *Service) OnlineStatus(ctx context.Context) (json.RawMessage, error) {
	return s.onlineRPC(ctx, MsgOnlineStatus, struct{}{})
}

// PauseOnline is POST /api/online/pause: main's online.pause (website + call
// center; preset 15m|30m|60m|today).
func (s *Service) PauseOnline(ctx context.Context, preset, note string) (json.RawMessage, error) {
	if !pausePresets[preset] {
		return nil, fmt.Errorf("%w: preset must be 15m, 30m, 60m or today", ErrBadRequest)
	}
	return s.onlineRPC(ctx, MsgOnlinePause, map[string]string{"preset": preset, "note": strings.TrimSpace(note)})
}

// ResumeOnline is DELETE /api/online/pause: main's online.resume.
func (s *Service) ResumeOnline(ctx context.Context) (json.RawMessage, error) {
	return s.onlineRPC(ctx, MsgOnlineResume, struct{}{})
}

// MenuStock is GET /api/online/menu: main's menu.stock (items + toppings).
func (s *Service) MenuStock(ctx context.Context) (json.RawMessage, error) {
	return s.onlineRPC(ctx, MsgMenuStock, struct{}{})
}

// StockChange is a POST /api/online/stock body.
type StockChange struct {
	Kind    string `json:"kind"` // item | option
	ID      string `json:"id"`
	InStock *bool  `json:"in_stock"`
	Hold    bool   `json:"hold"` // sold out until turned back on (else back at the new day)
}

// SetStock is POST /api/online/stock: main's menu.stock.set.
func (s *Service) SetStock(ctx context.Context, c StockChange) (json.RawMessage, error) {
	if (c.Kind != "item" && c.Kind != "option") || strings.TrimSpace(c.ID) == "" || c.InStock == nil {
		return nil, fmt.Errorf("%w: kind item|option, id and in_stock are required", ErrBadRequest)
	}
	return s.onlineRPC(ctx, MsgMenuStockSet, map[string]any{"kind": c.Kind, "id": strings.TrimSpace(c.ID),
		"in_stock": *c.InStock, "hold": c.Hold})
}

// onlineRPC sends one online-ordering request to main and returns its reply
// payload: no answer / an error → ErrMainUnreachable; 202 (an older main) →
// ErrMainUnsupported; 400 → ErrBadRequest with main's message.
func (s *Service) onlineRPC(ctx context.Context, msgType string, payload any) (json.RawMessage, error) {
	ctx, span := s.span(ctx, "online.proxy", attribute.String("rpc.type", msgType))
	defer span.End()
	d := s.geoDeps()
	if d.Send == nil || (d.Linked != nil && !d.Linked()) {
		return nil, spanErr(span, ErrMainUnreachable)
	}
	rctx, cancel := context.WithTimeout(ctx, onlineWait)
	defer cancel()
	status, reply, err := d.Send(rctx, msgType, payload)
	switch {
	case err != nil:
		s.logger.Warn().Ctx(ctx).Err(err).Str("type", msgType).Msg("Online-ordering request not answered")
		return nil, spanErr(span, ErrMainUnreachable)
	case status == http.StatusAccepted: // main took the envelope but has no handler
		return nil, spanErr(span, ErrMainUnsupported)
	case status == http.StatusBadRequest:
		var e struct {
			Error string `json:"error"`
		}
		msg := "the server refused it"
		if json.Unmarshal(reply, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, spanErr(span, fmt.Errorf("%w: %s", ErrBadRequest, msg))
	case status != http.StatusOK || len(reply) == 0 || !json.Valid(reply):
		s.logger.Warn().Ctx(ctx).Int("status", status).Str("type", msgType).Msg("Online-ordering request refused")
		return nil, spanErr(span, ErrMainUnreachable)
	}
	return reply, nil
}
