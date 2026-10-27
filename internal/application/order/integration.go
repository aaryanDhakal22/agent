package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"quiccpos/agent/internal/application/devlog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// The store hub integration (2.1.0). The hub pulls checks from the store's
// old POS and PUTs them here as POS orders (local only: never synced to main
// as orders); it reads what it must act on (online accepts, runs sent,
// confirmed addresses, ...) from the integration feed. Settings (feature
// toggles, kitchen-ticket switches, default times) live in SQLite with an
// in-memory copy for the print path; the hub's status is in memory.

// Agent -> main geo RPCs for POS deliveries and ambiguous addresses.
const (
	MsgGeoLocate = "geo.locate"
	MsgGeoForget = "geo.forget"
	MsgGeoChoose = "geo.choose"
	// MsgGeoSearch (2.2.2): staff typed an address for an order; main
	// searches near the store and answers with order.geo (check / not_found).
	MsgGeoSearch = "geo.search"
)

var (
	// ErrPosPullOff: a POS upsert while features.pos_pull is off (409).
	ErrPosPullOff = errors.New("POS orders pull-in is off")
	// ErrNotFuture: schedule of an order that isn't a future order (409).
	ErrNotFuture = errors.New("not a future order")
	// ErrBadQuery: an address search that is empty or too long (400 bad_query).
	ErrBadQuery = errors.New("bad_query")
	// ErrTooSoon: main takes one address search per order every 3 s (429).
	ErrTooSoon = errors.New("one search at a time - try again in a moment")
)

// integrationState is the service's 2.1.0 runtime state.
type integrationState struct {
	settingsMu sync.Mutex // serializes settings writes
	hubMu      sync.Mutex
	hub        order.IntegrationStatus
	hubSaved   order.IntegrationStatus // last persisted (see SetIntegrationStatus)
	hubLink    bool                    // connected, as last published
	feedMu     sync.Mutex
	feedCh     chan struct{} // closed + replaced on every feed write
}

// SetStoreIDs gives the service the tenant + location this Pi is linked to
// (from store_config) for the orders it makes itself.
func (s *Service) SetStoreIDs(fn func() (tenantID, locationID string)) { s.storeIDs.Store(&fn) }

func (s *Service) currentStoreIDs() (string, string) {
	if fn := s.storeIDs.Load(); fn != nil && *fn != nil {
		t, l := (*fn)()
		if _, err := uuid.Parse(t); err == nil {
			if _, err := uuid.Parse(l); err == nil {
				return t, l
			}
		}
	}
	return uuid.Nil.String(), uuid.Nil.String()
}

// WaitBackground waits for background geo calls and POS prints (tests).
func (s *Service) WaitBackground() { s.bg.Wait() }

func (s *Service) background(ctx context.Context, fn func(context.Context)) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		fn(context.WithoutCancel(ctx))
	}()
}

// --- the feed -------------------------------------------------------------------

// feedNotify wakes long-polling feed readers (after any change that may have
// written a feed event).
func (s *Service) feedNotify() {
	s.integ.feedMu.Lock()
	if s.integ.feedCh != nil {
		close(s.integ.feedCh)
	}
	s.integ.feedCh = make(chan struct{})
	s.integ.feedMu.Unlock()
}

func (s *Service) feedWait() <-chan struct{} {
	s.integ.feedMu.Lock()
	defer s.integ.feedMu.Unlock()
	if s.integ.feedCh == nil {
		s.integ.feedCh = make(chan struct{})
	}
	return s.integ.feedCh
}

// FeedPage is GET /api/integration/events.
type FeedPage struct {
	Events []order.FeedEvent `json:"events"`
	Next   int64             `json:"next"`
}

// feedLimit caps one feed page.
const feedLimit = 200

// Feed returns the feed events after `after`, waiting up to wait for one when
// there are none yet. A cursor past the newest event (a fresh Pi after an SD
// swap) starts again from the oldest kept.
func (s *Service) Feed(ctx context.Context, after int64, wait time.Duration) (*FeedPage, error) {
	if after < 0 {
		after = 0
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		ch := s.feedWait()
		events, maxSeq, err := s.repo.ListFeed(ctx, after, feedLimit)
		if err != nil {
			if ctx.Err() != nil { // the reader went away
				return &FeedPage{Events: []order.FeedEvent{}, Next: after}, nil
			}
			return nil, err
		}
		if after > maxSeq {
			after = 0
			continue
		}
		if len(events) > 0 || wait <= 0 {
			page := &FeedPage{Events: events, Next: after}
			if len(events) > 0 {
				page.Next = events[len(events)-1].Seq
			}
			return page, nil
		}
		select {
		case <-ch:
		case <-time.After(time.Second): // a write from a path that didn't notify
		case <-deadline.C:
			wait = 0
		case <-ctx.Done():
			return &FeedPage{Events: []order.FeedEvent{}, Next: after}, nil
		}
	}
}

// --- settings ---------------------------------------------------------------------

// LoadIntegrationSettings refreshes the in-memory copy from the DB (startup,
// after a restore).
func (s *Service) LoadIntegrationSettings(ctx context.Context) {
	set, err := s.repo.IntegrationSettings(ctx)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("read integration settings failed; using defaults")
		set = order.DefaultIntegrationSettings()
	}
	s.integSet.Store(&set)
	if st, err := s.repo.IntegrationStatus(ctx); err == nil {
		s.integ.hubMu.Lock()
		s.integ.hub, s.integ.hubSaved = st, st
		s.integ.hubMu.Unlock()
	}
}

// IntegrationSettings are the features, ticket switches and default times in
// force (no DB read).
func (s *Service) IntegrationSettings() order.IntegrationSettings {
	if p := s.integSet.Load(); p != nil {
		return *p
	}
	return order.DefaultIntegrationSettings()
}

// UpdateIntegrationSettings applies change to the current settings, checks
// the times (1-240), stores them (writing the feed's settings.changed) and
// tells the tablets (SSE settings.updated + integration.status, whose
// insert_live follows features.order_insert).
func (s *Service) UpdateIntegrationSettings(ctx context.Context, change func(*order.IntegrationSettings) error) (order.IntegrationSettings, error) {
	s.integ.settingsMu.Lock()
	defer s.integ.settingsMu.Unlock()
	set := s.IntegrationSettings()
	if err := change(&set); err != nil {
		return set, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if err := set.Times.Validate(); err != nil {
		return set, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if err := s.repo.SetIntegrationSettings(ctx, set); err != nil {
		return set, err
	}
	s.integSet.Store(&set)
	s.feedNotify()
	if s.broker != nil {
		s.broker.Publish("settings.updated", set)
		s.broker.Publish("integration.status", s.IntegrationView(time.Now()))
	}
	s.logger.Info().Ctx(ctx).Interface("settings", set).Msg("Integration settings changed")
	return set, nil
}

// --- hub status --------------------------------------------------------------------

// IntegrationView is the hub's status as staff see it now.
func (s *Service) IntegrationView(now time.Time) order.IntegrationView {
	s.integ.hubMu.Lock()
	st := s.integ.hub
	s.integ.hubMu.Unlock()
	return st.View(now, s.IntegrationSettings().Features)
}

// InsertLive: the hub inserts online cash pickups into the POS right now
// (features.order_insert && insert_mode live && seen within 60 s).
func (s *Service) InsertLive() bool { return s.IntegrationView(time.Now()).InsertLive }

// hubPersistEvery bounds how often an unchanged status is written (SD wear).
const hubPersistEvery = 10 * time.Minute

// SetIntegrationStatus stores the hub's status PUT (in memory; persisted when
// it changed or every 10 min) and publishes SSE integration.status.
func (s *Service) SetIntegrationStatus(ctx context.Context, st order.IntegrationStatus) error {
	switch st.InsertMode {
	case "":
		st.InsertMode = order.InsertOff
	case order.InsertOff, order.InsertShadow, order.InsertLive:
	default:
		return fmt.Errorf("%w: insert_mode must be off, shadow or live", ErrBadRequest)
	}
	if st.Problems == nil {
		st.Problems = []order.Problem{}
	}
	for i, p := range st.Problems {
		switch p.Severity {
		case order.SeverityInfo, order.SeverityWarn, order.SeveritySerious:
		case "":
			st.Problems[i].Severity = order.SeverityWarn
		default:
			return fmt.Errorf("%w: problem severity must be info, warn or serious", ErrBadRequest)
		}
	}
	now := time.Now()
	st.SeenAt = now.UTC()
	s.integ.hubMu.Lock()
	prev := s.integ.hub
	s.integ.hub = st
	persist := !sameHubStatus(st, s.integ.hubSaved) || now.Sub(s.integ.hubSaved.SeenAt) > hubPersistEvery
	if persist {
		s.integ.hubSaved = st
	}
	wasLinked := s.integ.hubLink
	s.integ.hubLink = true
	s.integ.hubMu.Unlock()
	if !wasLinked {
		s.devEvent(devlog.Info, devlog.KindHub, "Store hub connected (insert mode %s)", st.InsertMode)
	}
	if persist {
		if err := s.repo.SetIntegrationStatus(ctx, st); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("persist integration status failed (kept in memory)")
		}
	}
	if s.broker != nil && (!sameHubStatus(st, prev) || now.Sub(prev.SeenAt) > order.IntegrationStaleAfter) {
		s.broker.Publish("integration.status", s.IntegrationView(now))
	}
	return nil
}

func sameHubStatus(a, b order.IntegrationStatus) bool {
	ja, _ := json.Marshal(order.IntegrationStatus{OK: a.OK, InsertMode: a.InsertMode, OperationDay: a.OperationDay, Problems: a.Problems, Version: a.Version})
	jb, _ := json.Marshal(order.IntegrationStatus{OK: b.OK, InsertMode: b.InsertMode, OperationDay: b.OperationDay, Problems: b.Problems, Version: b.Version})
	return string(ja) == string(jb)
}

// CheckIntegrationLink publishes integration.status once when the hub goes
// quiet (no PUT for 60 s): the tablets show "POS link down". Run on the
// escalation ticker.
func (s *Service) CheckIntegrationLink(now time.Time) {
	v := s.IntegrationView(now)
	s.integ.hubMu.Lock()
	changed := s.integ.hubLink && !v.Connected
	if changed {
		s.integ.hubLink = false
	}
	s.integ.hubMu.Unlock()
	if changed {
		s.devEvent(devlog.Warn, devlog.KindHub, "POS link down: the store hub stopped reporting")
		s.logger.Warn().Msg("POS link down: the store hub stopped reporting")
		if s.broker != nil {
			s.broker.Publish("integration.status", v)
		}
	}
}

// --- POS orders ---------------------------------------------------------------------

// UpsertResult is PUT /api/integration/orders/{ext_ref}'s answer.
type UpsertResult struct {
	OrderID string `json:"order_id"`
	Created bool   `json:"created"`
	Changed bool   `json:"changed"`
}

// UpsertExternal stores a POS check (see contract A4): new → accepted with
// the default prep from Send, its station tickets per the POS switches (never
// a receipt), SSE order.added (a chime for a shown delivery), and main is
// asked to locate a delivery; changed → updated in place (no reprint),
// re-located when the address changed; same hash → nothing. A driver number
// newly set at the terminal is mirrored.
func (s *Service) UpsertExternal(ctx context.Context, extRef string, in order.ExternalOrder) (*UpsertResult, error) {
	ctx, span := s.span(ctx, "integration.upsert_order", attribute.String("order.ext_ref", extRef))
	defer span.End()
	set := s.IntegrationSettings()
	if !set.Features.PosPull {
		return nil, spanErr(span, ErrPosPullOff)
	}
	extRef = strings.TrimSpace(extRef)
	if extRef == "" || utf8.RuneCountInString(extRef) > 64 {
		return nil, spanErr(span, fmt.Errorf("%w: ext_ref must be 1-64 characters", ErrBadRequest))
	}
	if err := in.Validate(); err != nil {
		return nil, spanErr(span, fmt.Errorf("%w: %w", ErrBadRequest, err))
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, spanErr(span, err)
	}
	tenant, location := s.currentStoreIDs()
	res, err := s.repo.UpsertExternal(ctx, extRef, in, order.ExternalMeta{OrderID: id.String(), TenantID: tenant,
		LocationID: location, PrepMinutes: set.Times.For(in.CheckType), Now: time.Now()})
	if err != nil {
		return nil, spanErr(span, err)
	}
	so := res.Order
	span.SetAttributes(attribute.String("order.id", so.Order.ID), attribute.Bool("created", res.Created), attribute.Bool("changed", res.Changed))
	switch {
	case res.Created:
		s.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Str("ext_ref", extRef).Str("type", in.CheckType).Msg("POS order pulled")
		if s.broker != nil {
			s.broker.Publish("order.added", map[string]any{"order": so, "chime": so.IsDelivery() && !so.Status.BoardHidden})
		}
		if set.Tickets.POSFor(in.CheckType) {
			s.printPOS(ctx, so)
		}
		s.geoLocate(ctx, so)
	case res.Changed:
		s.publishUpdated(so)
		switch {
		case res.AddressChanged: // a new address, or newly a delivery
			s.geoLocate(ctx, so)
		case res.DeliveryEnded: // no longer a delivery: out of its pool
			s.geoForget(ctx, so.Order.ID)
			for _, x := range s.resequencePools(ctx, nil) {
				s.publishUpdated(x)
			}
		}
	}
	pos := ""
	if p := in.DriverPosNumber; p != nil {
		pos = strings.TrimSpace(*p)
	}
	switch prev := res.PrevDriverPos; {
	case strings.HasPrefix(prev, order.CameBackMark):
		// Came back (2.2.1): the POS still showing the driver it came back
		// from is not mirrored again; once it shows none, any later number
		// mirrors like a first one.
		switch {
		case pos == "":
			if _, err := s.repo.SetExtDriver(ctx, so.Order.ID, "", ""); err != nil {
				s.logger.Warn().Ctx(ctx).Err(err).Msg("forget the came-back POS driver number failed")
			}
		case pos != strings.TrimPrefix(prev, order.CameBackMark):
			s.mirrorDriver(ctx, so, pos)
		}
	case pos != "" && pos != prev:
		s.mirrorDriver(ctx, so, pos)
	}
	s.feedNotify()
	return &UpsertResult{OrderID: so.Order.ID, Created: res.Created, Changed: res.Changed}, nil
}

// printPOS prints a new POS order's station tickets in the background (the
// hub mustn't wait on a dead printer); nothing printed only raises an alert.
func (s *Service) printPOS(ctx context.Context, so order.StoredOrder) {
	s.background(ctx, func(ctx context.Context) {
		start := time.Now()
		out, err := s.print(ctx, printable(so), printer.ModeAccept)
		s.recordPrintTime(ctx, so, start, out, err)
		if errors.Is(err, printer.ErrNoTickets) {
			return
		}
		s.alertPrintIssues(ctx, so, out, false)
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", so.Order.ID).Msg("POS order: no kitchen ticket printed")
		} else {
			s.printEvents(label(so)+" (POS)", out)
		}
	})
}

// mirrorDriver mirrors a driver the cashier set on a POS delivery: available
// → sent out alone (run.sent); already out → joins that run; off or unknown →
// a note on the order and a warn toast. The number is remembered either way
// (the next change of it is mirrored again).
func (s *Service) mirrorDriver(ctx context.Context, so order.StoredOrder, pos string) {
	ctx, span := s.span(ctx, "integration.mirror_driver", attribute.String("order.id", so.Order.ID), attribute.String("driver.pos_number", pos))
	defer span.End()
	note := ""
	switch d, err := s.repo.DriverByPosNumber(ctx, pos); {
	case !so.IsDelivery() || so.IsFinished() || so.Status.State == order.StateOnDelivery:
		// Nothing to mirror (not a delivery, or already gone).
	case err != nil:
		note = fmt.Sprintf("POS says #%s (no driver has that number)", pos)
	case d.Status == order.DriverOff:
		note = fmt.Sprintf("POS says #%s (%s, not clocked in)", pos, d.Name)
	case d.Status == order.DriverAvailable:
		sent, readied, unpooled, err := s.repo.MirrorSendOut(ctx, d.ID, so.Order.ID)
		if err != nil {
			note = fmt.Sprintf("POS says #%s (%s) — couldn't send it out: %v", pos, d.Name, err)
			break
		}
		s.floorChanged()
		s.publishAll(latest(latest(readied, unpooled), sent))
		for _, x := range s.resequencePools(ctx, nil) {
			s.publishUpdated(x)
		}
		s.publishDrivers(ctx)
		s.publishDispatched(ctx, d.ID, sent)
		s.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Str("driver", d.Name).Msg("POS driver mirrored: sent out")
	case d.Status == order.DriverOut:
		joined, readied, err := s.repo.MirrorJoinRun(ctx, d.ID, so.Order.ID)
		if err != nil {
			note = fmt.Sprintf("POS says #%s (%s) — couldn't add it to the run: %v", pos, d.Name, err)
			break
		}
		s.floorChanged()
		s.publishAll(latest(readied, []order.StoredOrder{*joined}))
		for _, x := range s.resequencePools(ctx, nil) {
			s.publishUpdated(x)
		}
		s.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Str("driver", d.Name).Msg("POS driver mirrored: joined the run")
	}
	fresh, err := s.repo.SetExtDriver(ctx, so.Order.ID, pos, note)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("record the POS driver number failed")
		return
	}
	if note != "" {
		span.SetAttributes(attribute.String("note", note))
		s.publishUpdated(*fresh)
		s.Toast(order.SeverityWarn, note, so.Order.ID)
	}
}

// Toast shows a short message on the tablets (SSE integration.toast).
func (s *Service) Toast(severity, text, orderID string) {
	if s.broker == nil {
		return
	}
	m := map[string]any{"text": text, "severity": severity}
	if orderID != "" {
		m["order_id"] = orderID
	}
	s.broker.Publish("integration.toast", m)
}

// VoidExternal cancels a POS order voided at the terminal (out of any pool;
// main forgets a delivery's point).
func (s *Service) VoidExternal(ctx context.Context, extRef string) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "integration.void_order", attribute.String("order.ext_ref", extRef))
	defer span.End()
	so, changed, err := s.repo.VoidExternal(ctx, order.ExtSourceSoftTouch, strings.TrimSpace(extRef))
	if err != nil {
		return nil, spanErr(span, err)
	}
	if changed {
		s.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Str("ext_ref", extRef).Msg("POS order voided")
		s.publishUpdated(*so)
		for _, x := range s.resequencePools(ctx, nil) {
			s.publishUpdated(x)
		}
		if so.IsDelivery() {
			s.geoForget(ctx, so.Order.ID)
		}
		s.feedNotify()
	}
	return so, nil
}

// PatchOnline sets what the hub knows about an online order it inserted into
// the POS (or refused to): paid, its POS check number, a flag (nil clears).
func (s *Service) PatchOnline(ctx context.Context, id string, p order.OnlinePatch) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "integration.patch_online", attribute.String("order.id", id))
	defer span.End()
	if _, err := uuid.Parse(id); err != nil {
		return nil, spanErr(span, fmt.Errorf("%w: invalid order id", ErrBadRequest))
	}
	if f := p.Flag; f != nil {
		f.Text = strings.TrimSpace(f.Text)
		switch f.Severity {
		case "":
			f.Severity = order.SeverityWarn
		case order.SeverityInfo, order.SeverityWarn, order.SeveritySerious:
		default:
			return nil, spanErr(span, fmt.Errorf("%w: flag severity must be info, warn or serious", ErrBadRequest))
		}
		if f.Text == "" || utf8.RuneCountInString(f.Text) > 200 {
			return nil, spanErr(span, fmt.Errorf("%w: flag text must be 1-200 characters", ErrBadRequest))
		}
	}
	so, err := s.repo.PatchOnline(ctx, id, p)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.publishUpdated(*so)
	return so, nil
}

// --- geo for POS deliveries + ambiguous addresses -----------------------------------------

// geoLocate asks main to locate a POS delivery (address only, no name or
// phone); main answers later with order.geo. Unlinked / offline: skipped —
// RelocatePOS retries on the next connect.
func (s *Service) geoLocate(ctx context.Context, so order.StoredOrder) {
	a := so.Order.Fulfillment.Address
	if !so.IsPOS() || !so.IsDelivery() || a == nil || strings.TrimSpace(a.Line1) == "" {
		return
	}
	d := s.geoDeps()
	if d.Send == nil || (d.Linked != nil && !d.Linked()) {
		return
	}
	payload := map[string]any{"order_id": so.Order.ID, "address": map[string]string{
		"line1": a.Line1, "line2": a.Line2, "city": a.City, "state": a.State, "postal_code": a.PostalCode}}
	s.background(ctx, func(ctx context.Context) {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if status, _, err := d.Send(rctx, MsgGeoLocate, payload); err != nil || status != http.StatusOK {
			s.logger.Info().Ctx(ctx).Err(err).Int("status", status).Str("order_id", so.Order.ID).Msg("geo.locate not answered (retried on reconnect)")
		}
	})
}

// geoForget tells main to drop a POS delivery's point (void, close batch).
func (s *Service) geoForget(ctx context.Context, id string) {
	d := s.geoDeps()
	if d.Send == nil || (d.Linked != nil && !d.Linked()) {
		return
	}
	s.background(ctx, func(ctx context.Context) {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if status, _, err := d.Send(rctx, MsgGeoForget, map[string]any{"order_id": id}); err != nil || status != http.StatusOK {
			s.logger.Info().Ctx(ctx).Err(err).Int("status", status).Str("order_id", id).Msg("geo.forget not answered (main expires the point in 24 h)")
		}
	})
}

// RelocatePOS asks main again to locate every open POS delivery it hasn't
// answered for (after an internet drop). Run on every SSE connect.
func (s *Service) RelocatePOS(ctx context.Context) {
	open, err := s.repo.ListOpenDeliveries(ctx)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("relocate POS deliveries: list failed")
		return
	}
	for _, so := range open {
		if so.IsPOS() && (so.Geo == nil || so.Geo.State == order.GeoPending || so.Geo.State == order.GeoUnavailable) {
			s.geoLocate(ctx, so)
		}
	}
}

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
// with a note. Online orders and POS deliveries alike.
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

// --- future orders ---------------------------------------------------------------------------

// Schedule is a future order's first confirm (or a lead edit while it
// waits): no print, no accepted event; it waits in scheduled until
// promised_at - lead, then rings again.
func (s *Service) Schedule(ctx context.Context, id string, leadMinutes int) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.schedule", attribute.String("order.id", id), attribute.Int("lead_minutes", leadMinutes))
	defer span.End()
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, spanErr(span, fmt.Errorf("%w: invalid order id", ErrBadRequest))
	}
	if leadMinutes < 1 || leadMinutes > 240 {
		return nil, spanErr(span, fmt.Errorf("%w: lead_minutes must be 1-240", ErrBadRequest))
	}
	so, err := s.repo.GetByID(ctx, uid)
	if err != nil {
		return nil, spanErr(span, err)
	}
	if so.Status.State != order.StateArrived && so.Status.State != order.StateScheduled {
		return nil, spanErr(span, fmt.Errorf("%w (state %s)", order.ErrInvalidState, so.Status.State))
	}
	if !so.Status.Future || so.Order.Fulfillment.PromisedAt == nil {
		return nil, spanErr(span, ErrNotFuture)
	}
	ringAt := so.Order.Fulfillment.PromisedAt.Add(-time.Duration(leadMinutes) * time.Minute)
	out, err := s.repo.Schedule(ctx, uid.String(), int64(leadMinutes), ringAt)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.floorChanged()
	s.publishUpdated(*out)
	s.logger.Info().Ctx(ctx).Str("order_id", id).Int("lead_minutes", leadMinutes).Time("ring_at", ringAt).Msg("Future order scheduled")
	return out, nil
}

// RaiseDue puts scheduled orders whose ring time came back to arrived
// (rerung) and rings the tablets; the 2-minute escalation then applies. With
// auto-accept on, a raised order is accepted (and printed) right away, with
// its lead as the prep, as a new order would be. Run on the escalation ticker.
func (s *Service) RaiseDue(ctx context.Context, now time.Time) (int, error) {
	list, err := s.repo.RaiseDue(ctx, now)
	if err != nil {
		return 0, err
	}
	auto := false
	if len(list) > 0 {
		if auto, err = s.settings.GetAutoAccept(ctx); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("Failed to read auto-accept setting, defaulting to manual")
			auto = false
		}
	}
	for _, so := range list {
		if auto {
			id := uuid.MustParse(so.Order.ID)
			if _, err := s.AutoAccept(ctx, id); err == nil {
				s.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Msg("Future order is due: auto-accepted")
				continue
			}
			s.logger.Error().Ctx(ctx).Str("order_id", so.Order.ID).Msg("Auto-accept of a due future order failed — ringing")
			if cur, gerr := s.repo.GetByID(ctx, id); gerr == nil {
				if cur.Status.State != order.StateArrived {
					s.publishUpdated(*cur)
					continue
				}
				so = *cur
			}
		}
		s.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Msg("Future order is due: ringing again")
		s.broadcastArrived(ctx, so)
	}
	return len(list), nil
}
