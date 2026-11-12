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

	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// Addresses main couldn't place for sure (online and terminal deliveries):
// staff pick one of main's candidates or type an address to search for.
// Agent -> main RPCs; main answers later with order.geo.
const (
	MsgGeoChoose = "geo.choose"
	// MsgGeoSearch (2.2.2): staff typed an address for an order; main
	// searches near the store and answers with order.geo (check / not_found).
	MsgGeoSearch = "geo.search"
)

var (
	// ErrBadQuery: an address search that is empty or too long (400 bad_query).
	ErrBadQuery = errors.New("bad_query")
	// ErrTooSoon: main takes one address search per order every 3 s (429).
	ErrTooSoon = errors.New("one search at a time - try again in a moment")
)

// ChooseCandidate forwards staff's pick among an ambiguous address's
// candidates to main (geo.choose); main answers with order.geo located +
// confirmed_address.
func (s *Service) ChooseCandidate(ctx context.Context, id string, index int) error {
	ctx, span := s.span(ctx, "order.geo_choose", attribute.String("order.id", id), attribute.Int("index", index))
	defer span.End()
	uid, err := uuid.Parse(id)
	if err != nil {
		return spanErr(span, fmt.Errorf("%w: invalid order id", ErrBadRequest))
	}
	so, err := s.repo.GetByID(ctx, uid)
	if err != nil {
		return spanErr(span, err)
	}
	if so.Geo == nil || so.Geo.State != order.GeoCheck || len(so.Geo.Candidates) == 0 {
		return spanErr(span, fmt.Errorf("%w: the address has no candidates to choose from", order.ErrInvalidState))
	}
	if index < 0 || index >= len(so.Geo.Candidates) {
		return spanErr(span, fmt.Errorf("%w: index must be 0-%d", ErrBadRequest, len(so.Geo.Candidates)-1))
	}
	d := s.geoDeps()
	if d.Send == nil || (d.Linked != nil && !d.Linked()) {
		return spanErr(span, ErrMainOffline)
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, reply, err := d.Send(rctx, MsgGeoChoose, map[string]any{"order_id": uid.String(), "index": index})
	if err != nil || status != http.StatusOK {
		s.logger.Warn().Ctx(ctx).Err(err).Int("status", status).Msg("geo.choose not answered")
		return spanErr(span, ErrMainOffline)
	}
	var res struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(reply, &res)
	switch {
	case res.OK:
		return nil
	case res.Code == "not_found":
		return spanErr(span, order.ErrNotFound)
	default:
		return spanErr(span, fmt.Errorf("%w: the server didn't accept the choice (%s)", ErrBadRequest, res.Code))
	}
}

// SearchAddress forwards a staff address search for an order (typed on the
// tablet) to main (geo.search, 2.2.2). main answers {ok} at once and pushes
// order.geo later: check with up to 5 places near the store, or not_found
// with a note.
func (s *Service) SearchAddress(ctx context.Context, id, query string) error {
	ctx, span := s.span(ctx, "order.geo_search", attribute.String("order.id", id))
	defer span.End()
	uid, err := uuid.Parse(id)
	if err != nil {
		return spanErr(span, fmt.Errorf("%w: invalid order id", ErrBadRequest))
	}
	query = strings.Join(strings.Fields(query), " ")
	if query == "" || utf8.RuneCountInString(query) > order.MaxGeoSearchQuery {
		return spanErr(span, ErrBadQuery)
	}
	if _, err := s.repo.GetByID(ctx, uid); err != nil {
		return spanErr(span, err)
	}
	d := s.geoDeps()
	if d.Send == nil || (d.Linked != nil && !d.Linked()) {
		return spanErr(span, ErrMainOffline)
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, reply, err := d.Send(rctx, MsgGeoSearch, map[string]any{"order_id": uid.String(), "query": query})
	switch {
	case err != nil:
		s.logger.Warn().Ctx(ctx).Err(err).Msg("geo.search not answered")
		return spanErr(span, ErrMainOffline)
	case status == http.StatusAccepted: // main took the envelope but has no handler (< 2.2.2)
		return spanErr(span, ErrMainUnsupported)
	case status == http.StatusBadRequest:
		return spanErr(span, ErrBadQuery)
	case status != http.StatusOK:
		s.logger.Warn().Ctx(ctx).Int("status", status).Msg("geo.search refused")
		return spanErr(span, ErrMainOffline)
	}
	var res struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(reply, &res)
	switch {
	case res.OK:
		s.logger.Info().Ctx(ctx).Str("order_id", id).Msg("Address search sent to main")
		return nil
	case res.Code == "not_found":
		return spanErr(span, order.ErrNotFound)
	case res.Code == "too_soon":
		return spanErr(span, ErrTooSoon)
	default:
		return spanErr(span, fmt.Errorf("%w: the server didn't take the search (%s)", ErrBadRequest, res.Code))
	}
}
