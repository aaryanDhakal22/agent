package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/google/uuid"
)

// Terminal stage 0d: the snapshots main sends for the POS terminals, counter
// orders (placed, paid, voided), their payments and the action journal.
// Every change runs in one transaction with its floor events (the outbox to
// main: order.placed / order.payment / order.void / journal.entries).

// GetTerminalSnapshot is the stored snapshot of a kind (nil when none yet).
func (r *OrderRepository) GetTerminalSnapshot(ctx context.Context, kind string) (snap *terminal.Snapshot, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.get_snapshot")
	defer end(&err)
	row, err := models.New(r.db).GetTerminalSnapshot(ctx, kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	at, err := parseTS(row.FetchedAt)
	if err != nil {
		return nil, err
	}
	return &terminal.Snapshot{Kind: row.Kind, Version: row.Version, ETag: row.Etag, Body: []byte(row.Body), FetchedAt: at}, nil
}

// PutTerminalSnapshot replaces the stored snapshot of its kind.
func (r *OrderRepository) PutTerminalSnapshot(ctx context.Context, s terminal.Snapshot) (err error) {
	ctx, _, end := dbSpan(ctx, "terminal.put_snapshot")
	defer end(&err)
	return models.New(r.db).PutTerminalSnapshot(ctx, models.PutTerminalSnapshotParams{Kind: s.Kind, Version: s.Version,
		Etag: s.ETag, Body: string(s.Body), FetchedAt: fmtTS(s.FetchedAt)})
}

// CounterNext is the counter sequence's stored next number (0 = start).
func (r *OrderRepository) CounterNext(ctx context.Context) (n int64, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.counter_next")
	defer end(&err)
	return models.New(r.db).GetCounterNext(ctx)
}

// PlaceCounterOrder stores a counter order accepted at once with the next
// counter number, or returns the order its client_key already made (created
// false: nothing written).
func (r *OrderRepository) PlaceCounterOrder(ctx context.Context, in terminal.NewCounterOrder) (so order.StoredOrder, created bool, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.place_counter_order")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		switch row, err := q.GetOrderByClientKey(ctx, in.ClientKey); {
		case err == nil:
			so, err = toStored(row)
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		after, err := q.TakeCounterNumber(ctx, in.CounterStart)
		if err != nil {
			return fmt.Errorf("counter number: %w", err)
		}
		n := after - 1
		snap := in.Snapshot
		snap.KitchenNumber = &n
		raw, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		due, tips := snap.CashDue(), snap.Money.Tip
		now := in.Now
		ins := models.InsertCounterOrderParams{ID: snap.ID, TenantID: snap.TenantID, LocationID: snap.LocationID,
			Source: snap.Source, SourceEventID: snap.SourceEventID, Snapshot: string(raw), ArrivalDate: fmtTS(now),
			KitchenNumber:    sql.NullInt64{Int64: n, Valid: true},
			PrepMinutes:      sql.NullInt64{Int64: in.PrepMinutes, Valid: true},
			EstimatedReadyAt: nullTS(now.Add(time.Duration(in.PrepMinutes) * time.Minute)), AcceptedAt: nullTS(now),
			Paid: due == 0, CashDueCents: sql.NullInt64{Int64: due, Valid: true}, TipCents: sql.NullInt64{Int64: tips, Valid: true},
			SentAt: nullTS(now), ClientKey: in.ClientKey}
		if due == 0 {
			ins.PaidAt = nullTS(now)
		}
		if err := q.InsertCounterOrder(ctx, ins); err != nil {
			return fmt.Errorf("insert counter order: %w", err)
		}
		for _, p := range in.Payments {
			p.OrderID = snap.ID
			if err := insertPayment(ctx, q, p); err != nil {
				return err
			}
		}
		if so, err = loadOrder(ctx, q, snap.ID); err != nil {
			return err
		}
		created = true
		wire, err := placedSnapshot(so, in)
		if err != nil {
			return err
		}
		ev := map[string]any{"event_id": uuid.NewString(), "type": terminal.EventOrderPlaced, "order_id": snap.ID,
			"at": now.UTC().Truncate(time.Millisecond), "batch_id": so.Status.BatchID, "snapshot": wire,
			"agent_status": so.Status}
		if err := insertEvent(ctx, q, ev["event_id"].(string), snap.ID, terminal.EventOrderPlaced, now, ev); err != nil {
			return err
		}
		return insertJournal(ctx, q, now, in.Journal)
	})
	return so, created, err
}

// placedSnapshot is the order as main's order.placed takes it
// (contract/main.md §2.5zb): fulfillment counter | pickup | delivery (the
// Pi keeps a counter order as dine_in for the tablets), source_event_id =
// the client_key, the full payments, staff_id + device_id.
func placedSnapshot(so order.StoredOrder, in terminal.NewCounterOrder) (map[string]any, error) {
	raw, err := json.Marshal(so.Order)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if f, ok := m["fulfillment"].(map[string]any); ok && so.Order.Fulfillment.Type == terminal.FulfillmentOf(terminal.TypeCounter) {
		f["type"] = terminal.TypeCounter
	}
	pays := make([]map[string]any, 0, len(in.Payments))
	for _, p := range in.Payments {
		pays = append(pays, p.Wire())
	}
	m["payments"], m["source_event_id"], m["staff_id"], m["device_id"] = pays, in.ClientKey, in.StaffID, in.DeviceID
	if so.Status.EstimatedReadyAt != nil {
		m["estimated_ready_at"] = so.Status.EstimatedReadyAt.UTC()
	}
	return m, nil
}

func insertPayment(ctx context.Context, q *models.Queries, p terminal.Payment) error {
	err := q.InsertOrderPayment(ctx, models.InsertOrderPaymentParams{ID: p.ID, OrderID: p.OrderID, ClientKey: p.ClientKey,
		At: fmtTS(p.At), Tender: p.Tender, AmountCents: p.AmountCents, TipCents: p.TipCents, TenderedCents: p.TenderedCents,
		ChangeCents: p.ChangeCents, Refund: p.Refund, Reason: p.Reason, Ref: p.Ref, Last4: p.Last4, StaffID: p.StaffID,
		DeviceID: p.DeviceID, ApprovedBy: p.ApprovedBy})
	if err != nil {
		return fmt.Errorf("insert payment: %w", err)
	}
	return nil
}

func paymentFromRow(row models.OrderPayment) (terminal.Payment, error) {
	at, err := parseTS(row.At)
	if err != nil {
		return terminal.Payment{}, err
	}
	return terminal.Payment{ID: row.ID, OrderID: row.OrderID, ClientKey: row.ClientKey, At: at, Tender: row.Tender,
		AmountCents: row.AmountCents, TipCents: row.TipCents, TenderedCents: row.TenderedCents, ChangeCents: row.ChangeCents,
		Refund: row.Refund, Reason: row.Reason, Ref: row.Ref, Last4: row.Last4, StaffID: row.StaffID, DeviceID: row.DeviceID,
		ApprovedBy: row.ApprovedBy}, nil
}

func listPayments(ctx context.Context, q *models.Queries, orderID string) ([]terminal.Payment, error) {
	rows, err := q.ListOrderPayments(ctx, orderID)
	if err != nil {
		return nil, err
	}
	out := make([]terminal.Payment, 0, len(rows))
	for _, row := range rows {
		p, err := paymentFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// OrderPayments are a counter order's payments and refunds, oldest first.
func (r *OrderRepository) OrderPayments(ctx context.Context, orderID string) (out []terminal.Payment, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.order_payments")
	defer end(&err)
	return listPayments(ctx, models.New(r.db), orderID)
}

// CounterOrders are the open batch's counter orders, oldest first.
func (r *OrderRepository) CounterOrders(ctx context.Context) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.counter_orders")
	defer end(&err)
	q := models.New(r.db)
	b, err := q.GetOpenBatch(ctx)
	if err != nil {
		return nil, fmt.Errorf("open batch: %w", err)
	}
	rows, err := q.ListCounterOrders(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	return mapRowsGeo(ctx, q, rows)
}

// applyPayments appends payments to a counter order's snapshot (money
// columns too) and stores them. An online pickup's snapshot stays main's:
// only its money columns move (applyOnlinePayments).
func applyPayments(ctx context.Context, q *models.Queries, so order.StoredOrder, pays []terminal.Payment, now time.Time) (order.Snapshot, error) {
	if so.Order.Source != order.SourceCounter {
		return so.Order, applyOnlinePayments(ctx, q, so, pays, now)
	}
	snap := so.Order
	for _, p := range pays {
		if err := insertPayment(ctx, q, p); err != nil {
			return snap, err
		}
		amt, _ := p.Signed() // a tip taken with it stays on the payment (main's rule)
		snap.Payments = append(snap.Payments, order.PaymentSummary{Method: terminal.MethodOf(p.Tender), AmountCents: amt, Last4: p.Last4})
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return snap, err
	}
	due := snap.CashDue()
	paidAt := sql.NullString{}
	switch {
	case due == 0 && !so.Status.Paid:
		paidAt = nullTS(now)
	case due == 0 && so.Status.PaidAt != nil:
		paidAt = nullTS(*so.Status.PaidAt)
	}
	err = q.SetCounterMoney(ctx, models.SetCounterMoneyParams{Snapshot: string(raw), Paid: due == 0,
		CashDueCents: sql.NullInt64{Int64: due, Valid: true}, TipCents: sql.NullInt64{Int64: tipsOf(ctx, q, so.Order.ID), Valid: true}, PaidAt: paidAt, ID: so.Order.ID})
	return snap, err
}

// applyOnlinePayments stores payments a terminal took on an online pickup:
// the balance is the snapshot's cash due (the one cash rule) less what the
// terminals took; the snapshot itself is never rewritten.
func applyOnlinePayments(ctx context.Context, q *models.Queries, so order.StoredOrder, pays []terminal.Payment, now time.Time) error {
	for _, p := range pays {
		if err := insertPayment(ctx, q, p); err != nil {
			return err
		}
	}
	all, err := listPayments(ctx, q, so.Order.ID)
	if err != nil {
		return err
	}
	var net, tips int64
	for _, p := range all {
		a, t := p.Signed()
		net, tips = net+a, tips+t
	}
	due := max(so.Order.CashDue()-net, 0)
	paidAt := sql.NullString{}
	switch {
	case due == 0 && so.Status.PaidAt != nil:
		paidAt = nullTS(*so.Status.PaidAt)
	case due == 0:
		paidAt = nullTS(now)
	}
	return q.SetTerminalPaidMoney(ctx, models.SetTerminalPaidMoneyParams{Paid: due == 0,
		CashDueCents: sql.NullInt64{Int64: due, Valid: true}, TipCents: sql.NullInt64{Int64: tips, Valid: true}, PaidAt: paidAt, ID: so.Order.ID})
}

// tipsOf is the net tip taken with a counter order's payments.
func tipsOf(ctx context.Context, q *models.Queries, orderID string) int64 {
	pays, err := listPayments(ctx, q, orderID)
	if err != nil {
		return 0
	}
	var t int64
	for _, p := range pays {
		_, tip := p.Signed()
		t += tip
	}
	return t
}

// AddCounterPayment records a payment (or refund) on a counter order of the
// open batch. A payment whose client_key the order already has returns that
// payment (duplicate, nothing written).
func (r *OrderRepository) AddCounterPayment(ctx context.Context, orderID, clientKey string, check terminal.MoneyCheck) (so order.StoredOrder, pay terminal.Payment, duplicate bool, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.add_payment")
	defer end(&err)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		cur, err := loadOrder(ctx, q, orderID)
		if err != nil {
			return err
		}
		// A counter order, or (contract 2.10i) an online / website pickup
		// with cash to collect at the counter.
		if cur.Order.Source != order.SourceCounter && !terminal.IsPickup(cur) {
			return terminal.ErrNotCounterOrder
		}
		if clientKey != "" {
			switch row, err := q.GetOrderPaymentByKey(ctx, models.GetOrderPaymentByKeyParams{OrderID: orderID, ClientKey: clientKey}); {
			case err == nil:
				duplicate, so = true, cur
				pay, err = paymentFromRow(row)
				return err
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return fmt.Errorf("open batch: %w", err)
		}
		if cur.Status.BatchID != b.ID || cur.Status.State == order.StateCancelled {
			return terminal.ErrNotPayable
		}
		if cur.Order.Source != order.SourceCounter && !terminal.OnPickupStates(cur) {
			return terminal.ErrNotPayable // an online order still ringing, picked up, ...
		}
		paid, err := listPayments(ctx, q, orderID)
		if err != nil {
			return err
		}
		pays, entries, err := check(cur, paid)
		if err != nil {
			return err
		}
		if len(pays) != 1 {
			return fmt.Errorf("one payment expected, got %d", len(pays))
		}
		pay = pays[0]
		if _, err := applyPayments(ctx, q, cur, pays, now); err != nil {
			return err
		}
		if so, err = loadOrder(ctx, q, orderID); err != nil {
			return err
		}
		if err := recordPayment(ctx, q, so, pay, now); err != nil {
			return err
		}
		return insertJournal(ctx, q, now, entries)
	})
	return so, pay, duplicate, err
}

// recordPayment writes main's order.payment {order_id, payment} (a refund
// negative), plus the Pi's balance for the record.
func recordPayment(ctx context.Context, q *models.Queries, so order.StoredOrder, p terminal.Payment, now time.Time) error {
	id := uuid.NewString()
	ev := map[string]any{"event_id": id, "type": terminal.EventOrderPayment, "order_id": so.Order.ID,
		"at": now.UTC().Truncate(time.Millisecond), "payment": p.Wire(),
		"balance_cents": so.Status.CashDueCents, "paid": so.Status.Paid}
	return insertEvent(ctx, q, id, so.Order.ID, terminal.EventOrderPayment, now, ev)
}

// VoidCounterOrder voids a counter order of the open batch (accepted, ready
// or out; off its pool / run) and stores the refunds check decides.
func (r *OrderRepository) VoidCounterOrder(ctx context.Context, id, reason, staffID, deviceID string, check terminal.MoneyCheck) (so, prev order.StoredOrder, refunds []terminal.Payment, returned bool, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.void_counter_order")
	defer end(&err)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		cur, err := loadOrder(ctx, q, id)
		if err != nil {
			return err
		}
		prev = cur
		if cur.Order.Source != order.SourceCounter {
			return terminal.ErrNotCounterOrder
		}
		if cur.Status.State == order.StateCancelled && cur.Status.CancelReason == terminal.VoidReason {
			so = cur
			return terminal.ErrAlreadyVoided
		}
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return fmt.Errorf("open batch: %w", err)
		}
		if cur.Status.BatchID != b.ID {
			return terminal.ErrNotVoidable
		}
		switch cur.Status.State {
		case order.StateAccepted, order.StateReady, order.StateOnDelivery:
		default:
			return terminal.ErrNotVoidable
		}
		paid, err := listPayments(ctx, q, id)
		if err != nil {
			return err
		}
		var entries []terminal.Entry
		if refunds, entries, err = check(cur, paid); err != nil {
			return err
		}
		if len(refunds) > 0 {
			if _, err := applyPayments(ctx, q, cur, refunds, now); err != nil {
				return err
			}
		}
		n, err := q.CancelChannelOrder(ctx, models.CancelChannelOrderParams{CancelledAt: nullTS(now), Reason: terminal.VoidReason, ID: id})
		if err != nil {
			return fmt.Errorf("void order: %w", err)
		}
		if n == 0 {
			return terminal.ErrNotVoidable
		}
		if cur.Status.State == order.StateOnDelivery && cur.Status.DriverID != "" {
			drv, err := q.GetDriver(ctx, cur.Status.DriverID)
			switch {
			case err == nil:
				if returned, err = returnIfRunDone(ctx, q, drv, now); err != nil {
					return err
				}
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		if so, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		// main: the refunds are negative order.payment events, then the void.
		for _, p := range refunds {
			if err := recordPayment(ctx, q, so, p, now); err != nil {
				return err
			}
		}
		evID := uuid.NewString()
		ev := map[string]any{"event_id": evID, "type": terminal.EventOrderVoid, "order_id": id,
			"at": now.UTC().Truncate(time.Millisecond), "reason": reason, "staff_id": nilIfEmpty(staffID)}
		if len(refunds) > 0 && refunds[0].ApprovedBy != "" {
			ev["approved_by"] = refunds[0].ApprovedBy
		}
		if err := insertEvent(ctx, q, evID, id, terminal.EventOrderVoid, now, ev); err != nil {
			return err
		}
		return insertJournal(ctx, q, now, entries)
	})
	return so, prev, refunds, returned, err
}

// AddJournal stores journal rows (+ their journal.entries floor event).
func (r *OrderRepository) AddJournal(ctx context.Context, entries []terminal.Entry) (err error) {
	ctx, _, end := dbSpan(ctx, "terminal.add_journal")
	defer end(&err)
	return r.withTx(ctx, func(q *models.Queries) error { return insertJournal(ctx, q, time.Now(), entries) })
}

// insertJournal writes journal rows and one journal.entries event for them.
func insertJournal(ctx context.Context, q *models.Queries, now time.Time, entries []terminal.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	for i := range entries {
		e := &entries[i]
		if e.ID == "" {
			e.ID = uuid.NewString()
		}
		if e.At.IsZero() {
			e.At = now
		}
		e.At = e.At.UTC().Truncate(time.Millisecond)
		if len(e.Detail) == 0 {
			e.Detail = json.RawMessage("{}")
		}
		if err := q.InsertJournal(ctx, models.InsertJournalParams{ID: e.ID, At: fmtTS(e.At), StaffID: e.StaffID,
			DeviceID: e.DeviceID, Action: e.Action, OrderID: e.OrderID, Detail: string(e.Detail)}); err != nil {
			return fmt.Errorf("insert journal: %w", err)
		}
	}
	// main wants uuids or null for staff_id / order_id.
	wire := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		wire = append(wire, map[string]any{"id": e.ID, "at": e.At, "staff_id": nilIfEmpty(e.StaffID), "device_id": e.DeviceID,
			"action": e.Action, "order_id": nilIfEmpty(e.OrderID), "detail": e.Detail})
	}
	id := uuid.NewString()
	ev := map[string]any{"event_id": id, "type": terminal.EventJournalEntries, "at": now.UTC().Truncate(time.Millisecond),
		"entries": wire}
	return insertEvent(ctx, q, id, "", terminal.EventJournalEntries, now, ev)
}

// Journal is the newest journal rows first (at most limit).
func (r *OrderRepository) Journal(ctx context.Context, limit int) (out []terminal.Entry, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.journal")
	defer end(&err)
	rows, err := models.New(r.db).ListJournal(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	out = make([]terminal.Entry, 0, len(rows))
	for _, row := range rows {
		at, err := parseTS(row.At)
		if err != nil {
			return nil, err
		}
		out = append(out, terminal.Entry{ID: row.ID, At: at, StaffID: row.StaffID, DeviceID: row.DeviceID, Action: row.Action,
			OrderID: row.OrderID, Detail: json.RawMessage(row.Detail)})
	}
	return out, nil
}

// CounterOrderByKey is the order a terminal's client_key made (nil = none).
func (r *OrderRepository) CounterOrderByKey(ctx context.Context, key string) (so *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.order_by_key")
	defer end(&err)
	row, err := models.New(r.db).GetOrderByClientKey(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o, err := toStored(row)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
