package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

var _ order.Repository = (*OrderRepository)(nil)

type OrderRepository struct {
	db     *sql.DB
	logger zerolog.Logger
}

func NewOrderRepository(db *sql.DB, lg zerolog.Logger) *OrderRepository {
	return &OrderRepository{
		db:     db,
		logger: lg.With().Str("module", "order-repo").Logger(),
	}
}

func (r *OrderRepository) UpsertArrived(ctx context.Context, s order.Snapshot) (inserted bool, err error) {
	ctx, _, end := dbSpan(ctx, "orders.upsert_arrived")
	defer end(&err)

	// Validate the ids are UUIDs (they're stored as text, but a malformed id is
	// a bug worth surfacing early rather than persisting garbage).
	if _, err = uuid.Parse(s.ID); err != nil {
		return false, fmt.Errorf("invalid snapshot id %q: %w", s.ID, err)
	}
	if _, err = uuid.Parse(s.TenantID); err != nil {
		return false, fmt.Errorf("invalid tenant_id %q: %w", s.TenantID, err)
	}
	if _, err = uuid.Parse(s.LocationID); err != nil {
		return false, fmt.Errorf("invalid location_id %q: %w", s.LocationID, err)
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return false, fmt.Errorf("marshal snapshot: %w", err)
	}
	n, err := models.New(r.db).UpsertArrivedOrder(ctx, models.UpsertArrivedOrderParams{
		ID:            s.ID,
		TenantID:      s.TenantID,
		LocationID:    s.LocationID,
		Source:        s.Source,
		SourceEventID: s.SourceEventID,
		Snapshot:      string(payload),
	})
	return n == 1, err
}

func (r *OrderRepository) GetByID(ctx context.Context, id uuid.UUID) (so *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "orders.get_by_id")
	defer end(&err)

	stored, err := loadOrder(ctx, models.New(r.db), id.String())
	if err != nil {
		return nil, err
	}
	return &stored, nil
}

func (r *OrderRepository) ListPage(ctx context.Context, offset, limit int) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "orders.list_page")
	defer end(&err)

	q := models.New(r.db)
	rows, err := q.ListOrdersPage(ctx, models.ListOrdersPageParams{
		Limit:  int64(limit),
		Offset: int64(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	return mapRowsGeo(ctx, q, rows)
}

func (r *OrderRepository) ListArrived(ctx context.Context) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "orders.list_arrived")
	defer end(&err)

	q := models.New(r.db)
	rows, err := q.ListArrivedOrders(ctx)
	if err != nil {
		return nil, fmt.Errorf("list arrived: %w", err)
	}
	return mapRowsGeo(ctx, q, rows)
}

// ClaimAccept flips an arrived order to accepted in one transaction and
// returns it: allocates a kitchen number (unless a reverted earlier claim
// already did), stamps accepted_at + the prep estimate, and remembers the prep
// as the next default. Printing happens afterwards, outside any transaction
// (a slow or dead printer must not hold the single SQLite connection); the
// caller reverts with RevertAccept if nothing printed. Two concurrent accepts
// (mobile double-tap) can't both win: the loser's UPDATE matches 0 rows.
func (r *OrderRepository) ClaimAccept(ctx context.Context, id uuid.UUID, prepMinutes int, auto bool) (result *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "orders.claim_accept")
	defer end(&err)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	q := models.New(tx)

	row, err := q.GetOrderByID(ctx, id.String())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, order.ErrNotFound
		}
		return nil, fmt.Errorf("load order: %w", err)
	}
	if order.State(row.State) != order.StateArrived {
		return nil, order.ErrAlreadyAccepted
	}
	kitchen := row.KitchenNumber
	if !kitchen.Valid {
		next, err := q.NextKitchenNumber(ctx)
		if err != nil {
			return nil, fmt.Errorf("allocate kitchen number: %w", err)
		}
		kitchen = sql.NullInt64{Int64: next - 1, Valid: true}
	}
	now := time.Now()
	n, err := q.ClaimAccept(ctx, models.ClaimAcceptParams{
		Now:              nullTS(now),
		PrepMinutes:      sql.NullInt64{Int64: int64(prepMinutes), Valid: prepMinutes > 0},
		EstimatedReadyAt: nullTSIf(now.Add(time.Duration(prepMinutes)*time.Minute), prepMinutes > 0),
		KitchenNumber:    kitchen,
		AutoAccepted:     auto,
		ID:               id.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("claim accept: %w", err)
	}
	if n == 0 {
		return nil, order.ErrAlreadyAccepted
	}
	if prepMinutes > 0 {
		if err := q.SetLastPrepMinutes(ctx, int64(prepMinutes)); err != nil {
			return nil, fmt.Errorf("save prep default: %w", err)
		}
	}
	so, err := loadOrder(ctx, q, id.String()) // with its geo, like every order served
	if err != nil {
		return nil, fmt.Errorf("reload order: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &so, nil
}

// RevertAccept returns a claimed order to arrived (clearing printed_date).
func (r *OrderRepository) RevertAccept(ctx context.Context, id uuid.UUID) (err error) {
	ctx, _, end := dbSpan(ctx, "orders.revert_accept")
	defer end(&err)

	return models.New(r.db).RevertAccept(ctx, id.String())
}

// --- helpers --------------------------------------------------------------

func toStored(row models.Order) (order.StoredOrder, error) {
	var s order.Snapshot
	if err := json.Unmarshal([]byte(row.Snapshot), &s); err != nil {
		return order.StoredOrder{}, fmt.Errorf("unmarshal snapshot for order %s: %w", row.ID, err)
	}
	arrival, err := parseTS(row.ArrivalDate)
	if err != nil {
		return order.StoredOrder{}, fmt.Errorf("parse arrival_date for order %s: %w", row.ID, err)
	}
	st := order.Status{
		State:       order.State(row.State),
		ArrivalDate: arrival,
		BatchID:     row.BatchID,
		DriverName:  row.DriverName,
	}
	for _, f := range []struct {
		src sql.NullString
		dst **time.Time
	}{
		{row.PrintedDate, &st.PrintedDate},
		{row.EstimatedReadyAt, &st.EstimatedReadyAt},
		{row.AcceptedAt, &st.AcceptedAt},
		{row.ReadyAt, &st.ReadyAt},
		{row.OutAt, &st.OutAt},
		{row.DeliveredAt, &st.DeliveredAt},
		{row.CancelledAt, &st.CancelledAt},
		{row.PooledAt, &st.PooledAt},
		{row.SentAt, &st.SentAt},
		{row.RingAt, &st.RingAt},
	} {
		t, err := optTS(f.src)
		if err != nil {
			return order.StoredOrder{}, fmt.Errorf("parse timestamp for order %s: %w", row.ID, err)
		}
		*f.dst = t
	}
	if row.KitchenNumber.Valid {
		v := row.KitchenNumber.Int64
		st.KitchenNumber = &v
	}
	if row.PrepMinutes.Valid {
		v := row.PrepMinutes.Int64
		st.PrepMinutes = &v
	}
	if row.DriverID.Valid {
		st.DriverID = row.DriverID.String
	}
	if row.PoolDriverID.Valid {
		st.PoolDriverID = row.PoolDriverID.String
	}
	if row.PoolPlanID.Valid {
		st.PoolPlanID = row.PoolPlanID.String
	}
	if row.PoolSeq.Valid {
		v := row.PoolSeq.Int64
		st.PoolSeq = &v
	}
	// 2.1.0: POS orders, hub flags, future orders.
	st.ExtSource, st.ExtRef = row.ExtSource, row.ExtRef
	st.DisplayNumber = optInt(row.DisplayNumber)
	st.Paid, st.BoardHidden, st.LocalOnly, st.Rerung = row.Paid, row.BoardHidden, row.LocalOnly, row.Rerung
	st.AutoAccepted = row.AutoAccepted
	st.CameBack = row.CameBack
	st.RunSeq = optInt(row.RunSeq)
	if row.NotDeliveredAction != "" {
		at, err := optTS(row.NotDeliveredAt)
		if err != nil {
			return order.StoredOrder{}, fmt.Errorf("parse not_delivered_at for order %s: %w", row.ID, err)
		}
		nd := &order.NotDelivered{Action: row.NotDeliveredAction, DriverID: row.NotDeliveredDriverID}
		if at != nil {
			nd.At = at.UTC()
		}
		st.NotDelivered = nd
	}
	st.PosCheckNumber = optInt(row.PosCheckNumber)
	st.PosDriverNote = row.PosDriverNote
	st.ScheduledLeadMinutes = optInt(row.ScheduledLeadMinutes)
	if row.ExtSource != "" {
		st.CashDueCents, st.TipCents = row.CashDueCents.Int64, row.TipCents.Int64
	} else {
		st.CashDueCents = s.CashDue() // the one cash rule
	}
	if row.Flag != "" {
		var f order.Flag
		if json.Unmarshal([]byte(row.Flag), &f) == nil && f.Text != "" {
			st.Flag = &f
		}
	}
	// 00019: finishing + terminal changes.
	if st.FinishedAt, err = optTS(row.FinishedAt); err != nil {
		return order.StoredOrder{}, fmt.Errorf("parse finished_at for order %s: %w", row.ID, err)
	}
	if st.FinishedAt != nil {
		st.FinishedBy = row.FinishedBy
	}
	st.PosClosed = row.PosClosed
	if row.PosChange != "" {
		var c order.PosChange
		if json.Unmarshal([]byte(row.PosChange), &c) == nil && c.ID != "" {
			st.PosChange = &c
		}
	}
	return order.StoredOrder{Order: s, Status: st}, nil
}

func optInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

// geoFromRow converts an order_geo row to the wire shape.
func geoFromRow(row models.OrderGeo) *order.OrderGeo {
	g := &order.OrderGeo{State: row.State, Quality: row.Quality, DistanceM: row.DistanceM, DurationS: row.DurationS,
		Polyline: row.Polyline, OutsideArea: row.OutsideArea, Corrected: row.Corrected, Note: row.Note}
	if row.Lat.Valid && row.Lng.Valid {
		lat, lng := row.Lat.Float64, row.Lng.Float64
		g.Lat, g.Lng = &lat, &lng
	}
	if row.StoreToS.Valid {
		v := row.StoreToS.Int64
		g.StoreToS = &v
	}
	if row.ToStoreS.Valid {
		v := row.ToStoreS.Int64
		g.ToStoreS = &v
	}
	if t, err := parseTS(row.UpdatedAt); err == nil {
		g.UpdatedAt = t
	}
	if row.Candidates != "" {
		_ = json.Unmarshal([]byte(row.Candidates), &g.Candidates)
	}
	if row.ConfirmedAddress != "" {
		var a order.ConfirmedAddress
		if json.Unmarshal([]byte(row.ConfirmedAddress), &a) == nil {
			g.ConfirmedAddress = &a
		}
	}
	return g
}

// attachGeo fills StoredOrder.Geo for the orders main has located.
func attachGeo(ctx context.Context, q *models.Queries, list []order.StoredOrder) error {
	const chunk = 400
	idx := make(map[string]int, len(list))
	for i, so := range list {
		idx[so.Order.ID] = i
	}
	for start := 0; start < len(list); start += chunk {
		end := min(start+chunk, len(list))
		ids := make([]string, 0, end-start)
		for _, so := range list[start:end] {
			ids = append(ids, so.Order.ID)
		}
		rows, err := q.ListOrderGeo(ctx, ids)
		if err != nil {
			return fmt.Errorf("list order geo: %w", err)
		}
		for _, row := range rows {
			if i, ok := idx[row.OrderID]; ok {
				list[i].Geo = geoFromRow(row)
			}
		}
	}
	now := time.Now()
	for i := range list {
		list[i].FillMissingGeo(now)
	}
	return markFuture(ctx, q, list, now)
}

// markFuture computes Status.Future (it depends on the settings and the
// clock, so it is never stored).
func markFuture(ctx context.Context, q *models.Queries, list []order.StoredOrder, now time.Time) error {
	need := false
	for _, so := range list {
		need = need || (!so.IsPOS() && (so.Status.State == order.StateArrived || so.Status.State == order.StateScheduled))
	}
	if !need {
		return nil
	}
	set, err := integrationSettings(ctx, q)
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Status.Future = order.IsFuture(list[i], set, now)
	}
	return nil
}

// mapRowsGeo is mapRowsBare plus each order's geo. Every StoredOrder that
// leaves the repository (HTTP answers, SSE events) is built by loadOrder or
// mapRowsGeo; toStored / mapRowsBare are for internal reads only.
func mapRowsGeo(ctx context.Context, q *models.Queries, rows []models.Order) ([]order.StoredOrder, error) {
	out, err := mapRowsBare(rows)
	if err != nil {
		return nil, err
	}
	if err := attachGeo(ctx, q, out); err != nil {
		return nil, err
	}
	return out, nil
}

// mapRowsBare converts rows WITHOUT geo (internal reads; see mapRowsGeo).
func mapRowsBare(rows []models.Order) ([]order.StoredOrder, error) {
	out := make([]order.StoredOrder, 0, len(rows))
	for _, row := range rows {
		so, err := toStored(row)
		if err != nil {
			return nil, err
		}
		out = append(out, so)
	}
	return out, nil
}
