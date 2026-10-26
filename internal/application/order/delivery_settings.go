package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"quiccpos/agent/internal/domain/order"

	"go.opentelemetry.io/otel/attribute"
)

// Delivery settings (PUT /api/settings/deliveries): AUTO's two, the nearby
// rule (2.2.0) and the location's delivery radius, which lives on main
// (locations.delivery_radius_m) and is changed there over the duplex channel
// (RPC location.radius) — main re-flags outside_area, re-sends order.geo for
// the orders whose flag changed and pushes store_config.

// MsgLocationRadius is the agent -> main request that sets the delivery radius.
const MsgLocationRadius = "location.radius"

// ErrMainUnsupported: main answered but doesn't know the request (an older
// main; 501 main_unsupported).
var ErrMainUnsupported = errors.New("the server doesn't support this yet")

// radiusWait is how long PUT /api/settings/deliveries waits for main.
const radiusWait = 10 * time.Second

// DeliveryPatch is a PUT /api/settings/deliveries body: any subset.
type DeliveryPatch struct {
	SoonReadyMinutes      *int64   `json:"soon_ready_minutes"`
	DeliveryTargetMinutes *int64   `json:"delivery_target_minutes"`
	NearbyRule            *string  `json:"nearby_rule"`
	NearbyDetourMinutes   *int64   `json:"nearby_detour_minutes"`
	NearbyDistanceMi      *float64 `json:"nearby_distance_mi"`
	DeliveryRadiusM       *int64   `json:"delivery_radius_m"`
}

// DeliverySettings is the full settings object PUT answers with.
type DeliverySettings struct {
	order.DeliveryPrefs
	// DeliveryRadiusM is the location's radius (store_config; null before
	// the first handshake).
	DeliveryRadiusM *int64 `json:"delivery_radius_m"`
}

// DeliverySettings returns the delivery settings + the location's radius.
func (s *Service) DeliverySettings(ctx context.Context) DeliverySettings {
	out := DeliverySettings{DeliveryPrefs: s.DeliveryPrefs(ctx)}
	if d := s.geoDeps(); d.Radius != nil {
		if r, ok := d.Radius(); ok {
			out.DeliveryRadiusM = &r
		}
	}
	return out
}

// UpdateDeliverySettings applies a patch: everything is validated first; a
// radius goes to main (location.radius) before anything is saved, so when
// main can't take it nothing changes (ErrMainOffline / ErrMainUnsupported).
func (s *Service) UpdateDeliverySettings(ctx context.Context, p DeliveryPatch) (*DeliverySettings, error) {
	ctx, span := s.span(ctx, "settings.deliveries", attribute.Bool("radius", p.DeliveryRadiusM != nil))
	defer span.End()
	cur := s.DeliveryPrefs(ctx)
	next := cur
	if p.SoonReadyMinutes != nil {
		next.SoonReadyMinutes = *p.SoonReadyMinutes
	}
	if p.DeliveryTargetMinutes != nil {
		next.DeliveryTargetMinutes = *p.DeliveryTargetMinutes
	}
	if p.NearbyRule != nil {
		next.NearbyRule = *p.NearbyRule
	}
	if p.NearbyDetourMinutes != nil {
		next.NearbyDetourMinutes = *p.NearbyDetourMinutes
	}
	if p.NearbyDistanceMi != nil {
		next.NearbyDistanceMi = *p.NearbyDistanceMi
	}
	if err := next.Validate(); err != nil {
		return nil, spanErr(span, fmt.Errorf("%w: %w", ErrBadRequest, err))
	}
	if p.DeliveryRadiusM != nil {
		if r := *p.DeliveryRadiusM; r < order.MinDeliveryRadiusM || r > order.MaxDeliveryRadiusM {
			return nil, spanErr(span, fmt.Errorf("%w: delivery_radius_m must be %d-%d", ErrBadRequest, order.MinDeliveryRadiusM, order.MaxDeliveryRadiusM))
		}
		if err := s.setRadius(ctx, *p.DeliveryRadiusM); err != nil {
			return nil, spanErr(span, err)
		}
	}
	if next != cur {
		if err := s.repo.SetDeliveryPrefs(ctx, next); err != nil {
			return nil, spanErr(span, err)
		}
	}
	if s.broker != nil {
		s.broker.PublishFloorChanged()
	}
	out := s.DeliverySettings(ctx)
	if p.DeliveryRadiusM != nil {
		r := *p.DeliveryRadiusM
		out.DeliveryRadiusM = &r
	}
	return &out, nil
}

// setRadius asks main to set the location's delivery radius (10 s wait).
func (s *Service) setRadius(ctx context.Context, radiusM int64) error {
	d := s.geoDeps()
	if d.Send == nil || (d.Linked != nil && !d.Linked()) {
		return ErrMainOffline
	}
	rctx, cancel := context.WithTimeout(ctx, radiusWait)
	defer cancel()
	status, reply, err := d.Send(rctx, MsgLocationRadius, map[string]any{"radius_m": radiusM})
	switch {
	case err != nil:
		s.logger.Warn().Ctx(ctx).Err(err).Msg("location.radius not answered")
		return ErrMainOffline
	case status == http.StatusAccepted: // main took the envelope but has no handler
		return ErrMainUnsupported
	case status == http.StatusBadRequest:
		return fmt.Errorf("%w: the server refused the radius", ErrBadRequest)
	case status != http.StatusOK:
		s.logger.Warn().Ctx(ctx).Int("status", status).Msg("location.radius refused")
		return ErrMainOffline
	}
	var res struct {
		RadiusM *int64 `json:"radius_m"`
	}
	if json.Unmarshal(reply, &res) != nil || res.RadiusM == nil {
		return ErrMainOffline
	}
	if d.SetRadius != nil {
		d.SetRadius(*res.RadiusM) // until main's store_config push lands
	}
	s.logger.Info().Ctx(ctx).Int64("radius_m", *res.RadiusM).Msg("Delivery radius set on main")
	return nil
}
