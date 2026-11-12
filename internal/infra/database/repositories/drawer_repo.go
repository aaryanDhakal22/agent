package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/google/uuid"
)

// Cash drawers (contract/agent.md §2.10k): one session per terminal and
// cashier shift. Every change is one transaction with its floor event
// (drawer.opened / drawer.movement / drawer.closed, subject "drawer:<id>")
// and its journal rows. A cash payment goes in the paying terminal's open
// session (insertPayment).

func drawerSubject(id string) string { return "drawer:" + id }

func drawerTotals(ctx context.Context, q *models.Queries, row models.DrawerSession) (terminal.DrawerTotals, error) {
	var t terminal.DrawerTotals
	cash, err := q.SumDrawerCash(ctx, row.ID)
	if err != nil {
		return t, err
	}
	mv, err := q.SumDrawerMovements(ctx, row.ID)
	if err != nil {
		return t, err
	}
	to := ""
	if row.ClosedAt.Valid {
		to = row.ClosedAt.String
	}
	noSales, err := q.CountNoSales(ctx, models.CountNoSalesParams{DeviceID: row.DeviceID, FromAt: row.OpenedAt, ToAt: to})
	if err != nil {
		return t, err
	}
	t = terminal.DrawerTotals{CashSalesCents: cash.SalesCents, CashTipsCents: cash.TipsCents, CashRefundsCents: cash.RefundsCents,
		PayInsCents: mv.PayInsCents, PayOutsCents: mv.PayOutsCents, Payments: cash.Payments, NoSales: noSales}
	return t, nil
}

func drawerOf(ctx context.Context, q *models.Queries, row models.DrawerSession, withMoves bool) (terminal.Drawer, error) {
	d := terminal.Drawer{ID: row.ID, DeviceID: row.DeviceID, DeviceName: row.DeviceName, StaffID: row.StaffID, StaffName: row.StaffName,
		BatchID: row.BatchID, StartCents: row.StartCents, HandoverFrom: row.HandoverFrom, ClosedBy: row.ClosedBy,
		CloseReason: row.CloseReason, Flagged: row.Flagged, Auto: row.Auto}
	var err error
	if d.OpenedAt, err = parseTS(row.OpenedAt); err != nil {
		return d, err
	}
	if row.ClosedAt.Valid {
		at, err := parseTS(row.ClosedAt.String)
		if err != nil {
			return d, err
		}
		d.ClosedAt = &at
	}
	if row.CountedCents.Valid {
		v := row.CountedCents.Int64
		d.CountedCents = &v
	}
	if row.Denominations != "" {
		_ = json.Unmarshal([]byte(row.Denominations), &d.Denominations)
	}
	if d.DrawerTotals, err = drawerTotals(ctx, q, row); err != nil {
		return d, err
	}
	if row.ExpectedCents.Valid {
		d.ExpectedCents = row.ExpectedCents.Int64
	}
	d.Finish()
	if withMoves {
		rows, err := q.ListDrawerMovements(ctx, row.ID)
		if err != nil {
			return d, err
		}
		d.Movements = make([]terminal.Movement, 0, len(rows))
		for _, mr := range rows {
			m, err := movementOf(mr)
			if err != nil {
				return d, err
			}
			d.Movements = append(d.Movements, m)
		}
	}
	return d, nil
}

func movementOf(row models.DrawerMovement) (terminal.Movement, error) {
	at, err := parseTS(row.At)
	return terminal.Movement{ID: row.ID, DrawerID: row.DrawerID, Kind: row.Kind, AmountCents: row.AmountCents, Reason: row.Reason,
		StaffID: row.StaffID, ApprovedBy: row.ApprovedBy, ClientKey: row.ClientKey, At: at}, err
}

func optDrawer(ctx context.Context, q *models.Queries, row models.DrawerSession, err error, withMoves bool) (*terminal.Drawer, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := drawerOf(ctx, q, row, withMoves)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// DrawerByDevice is a terminal's open drawer (nil = none).
func (r *OrderRepository) DrawerByDevice(ctx context.Context, deviceID string) (d *terminal.Drawer, err error) {
	ctx, _, end := dbSpan(ctx, "drawers.by_device")
	defer end(&err)
	q := models.New(r.db)
	row, err := q.GetOpenDrawerByDevice(ctx, deviceID)
	return optDrawer(ctx, q, row, err, true)
}

// DrawerByStaff is a staff member's open drawer on any terminal (nil = none).
func (r *OrderRepository) DrawerByStaff(ctx context.Context, staffID string) (d *terminal.Drawer, err error) {
	ctx, _, end := dbSpan(ctx, "drawers.by_staff")
	defer end(&err)
	q := models.New(r.db)
	row, err := q.GetOpenDrawerByStaff(ctx, staffID)
	return optDrawer(ctx, q, row, err, false)
}

// DrawerByID is one session with its movements (terminal.ErrDrawerNotFound).
func (r *OrderRepository) DrawerByID(ctx context.Context, id string) (d *terminal.Drawer, err error) {
	ctx, _, end := dbSpan(ctx, "drawers.by_id")
	defer end(&err)
	q := models.New(r.db)
	row, err := q.GetDrawerSession(ctx, id)
	if d, err = optDrawer(ctx, q, row, err, true); err == nil && d == nil {
		err = terminal.ErrDrawerNotFound
	}
	return d, err
}

// DrawersByBatch are a batch's sessions (0 = the open batch), oldest first.
func (r *OrderRepository) DrawersByBatch(ctx context.Context, batchID int64) (out []terminal.Drawer, err error) {
	ctx, _, end := dbSpan(ctx, "drawers.by_batch")
	defer end(&err)
	q := models.New(r.db)
	return drawersByBatch(ctx, q, batchID)
}

func drawersByBatch(ctx context.Context, q *models.Queries, batchID int64) ([]terminal.Drawer, error) {
	if batchID == 0 {
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return nil, fmt.Errorf("open batch: %w", err)
		}
		batchID = b.ID
	}
	rows, err := q.ListDrawersByBatch(ctx, batchID)
	if err != nil {
		return nil, err
	}
	out := make([]terminal.Drawer, 0, len(rows))
	for _, row := range rows {
		d, err := drawerOf(ctx, q, row, true)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// openDrawerTx opens a session in the open batch: one per terminal, one per
// staff member.
func openDrawerTx(ctx context.Context, q *models.Queries, d *terminal.Drawer, journal []terminal.Entry, at time.Time) error {
	if row, err := q.GetOpenDrawerByDevice(ctx, d.DeviceID); err == nil {
		cur, err := drawerOf(ctx, q, row, false)
		if err != nil {
			return err
		}
		return &terminal.DrawerOpen{Drawer: cur}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if row, err := q.GetOpenDrawerByStaff(ctx, d.StaffID); err == nil && !d.Auto {
		cur, err := drawerOf(ctx, q, row, false)
		if err != nil {
			return err
		}
		return &terminal.CashierHasDrawer{Drawer: cur}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	b, err := q.GetOpenBatch(ctx)
	if err != nil {
		return fmt.Errorf("open batch: %w", err)
	}
	d.BatchID = b.ID
	if err := q.InsertDrawerSession(ctx, models.InsertDrawerSessionParams{ID: d.ID, DeviceID: d.DeviceID, DeviceName: d.DeviceName,
		StaffID: d.StaffID, StaffName: d.StaffName, BatchID: d.BatchID, OpenedAt: fmtTS(d.OpenedAt), StartCents: d.StartCents,
		HandoverFrom: d.HandoverFrom, Auto: d.Auto}); err != nil {
		return fmt.Errorf("open drawer: %w", err)
	}
	d.Finish()
	evID := uuid.NewString()
	ev := map[string]any{"event_id": evID, "type": terminal.EventDrawerOpened, "at": at.UTC().Truncate(time.Millisecond), "drawer": d.Wire()}
	if err := insertSubjectEvent(ctx, q, evID, "", drawerSubject(d.ID), terminal.EventDrawerOpened, at, ev); err != nil {
		return err
	}
	return insertJournal(ctx, q, at, journal)
}

// OpenDrawer opens a session (d.ID, device, staff, start, opened_at set by
// the caller): *terminal.DrawerOpen / *terminal.CashierHasDrawer.
func (r *OrderRepository) OpenDrawer(ctx context.Context, d terminal.Drawer, journal []terminal.Entry) (out terminal.Drawer, err error) {
	ctx, _, end := dbSpan(ctx, "drawers.open")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		return openDrawerTx(ctx, q, &d, journal, d.OpenedAt)
	})
	return d, err
}

// AddDrawerMovement records a pay-in / pay-out on the terminal's open
// drawer. check (inside the transaction) sees the drawer and returns the
// journal rows or an error (limits, not enough cash). A client_key the drawer
// has answers the stored movement (duplicate).
func (r *OrderRepository) AddDrawerMovement(ctx context.Context, deviceID string, m terminal.Movement,
	check func(d terminal.Drawer) (terminal.Movement, []terminal.Entry, error)) (d terminal.Drawer, got terminal.Movement, dup bool, err error) {
	ctx, _, end := dbSpan(ctx, "drawers.movement")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		row, err := q.GetOpenDrawerByDevice(ctx, deviceID)
		if errors.Is(err, sql.ErrNoRows) {
			return terminal.ErrNoDrawer
		}
		if err != nil {
			return err
		}
		if m.ClientKey != "" {
			switch mr, err := q.GetDrawerMovementByKey(ctx, models.GetDrawerMovementByKeyParams{DrawerID: row.ID, ClientKey: m.ClientKey}); {
			case err == nil:
				if got, err = movementOf(mr); err != nil {
					return err
				}
				if got.Kind != m.Kind || got.AmountCents != m.AmountCents {
					return terminal.ErrMovementKeyReused
				}
				dup = true
				d, err = drawerOf(ctx, q, row, true)
				return err
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		cur, err := drawerOf(ctx, q, row, false)
		if err != nil {
			return err
		}
		m.DrawerID = row.ID
		mv, journal, err := check(cur)
		if err != nil {
			return err
		}
		got = mv
		if err := q.InsertDrawerMovement(ctx, models.InsertDrawerMovementParams{ID: got.ID, DrawerID: got.DrawerID, Kind: got.Kind,
			AmountCents: got.AmountCents, Reason: got.Reason, StaffID: got.StaffID, ApprovedBy: got.ApprovedBy, ClientKey: got.ClientKey,
			At: fmtTS(got.At)}); err != nil {
			return fmt.Errorf("drawer movement: %w", err)
		}
		evID := uuid.NewString()
		ev := map[string]any{"event_id": evID, "type": terminal.EventDrawerMovement, "at": got.At.UTC().Truncate(time.Millisecond),
			"movement": got.Wire()}
		if err := insertSubjectEvent(ctx, q, evID, "", drawerSubject(row.ID), terminal.EventDrawerMovement, got.At, ev); err != nil {
			return err
		}
		if err := insertJournal(ctx, q, got.At, journal); err != nil {
			return err
		}
		d, err = drawerOf(ctx, q, row, true)
		return err
	})
	return d, got, dup, err
}

// closeDrawerTx counts a session out (expected from its cash flows now).
func closeDrawerTx(ctx context.Context, q *models.Queries, row models.DrawerSession, c terminal.DrawerClose) (terminal.Drawer, error) {
	cur, err := drawerOf(ctx, q, row, false)
	if err != nil {
		return cur, err
	}
	expected := cur.Expected(cur.StartCents)
	denoms := ""
	if len(c.Denominations) > 0 {
		b, _ := json.Marshal(c.Denominations)
		denoms = string(b)
	}
	counted := sql.NullInt64{}
	if c.CountedCents != nil {
		counted = sql.NullInt64{Int64: *c.CountedCents, Valid: true}
	}
	flagged := c.CountedCents == nil
	n, err := q.CloseDrawerSession(ctx, models.CloseDrawerSessionParams{ClosedAt: nullTS(c.At), ClosedBy: c.By, CloseReason: c.Reason,
		CountedCents: counted, Denominations: denoms, ExpectedCents: sql.NullInt64{Int64: expected, Valid: true}, Flagged: flagged, ID: row.ID})
	if err != nil {
		return cur, fmt.Errorf("close drawer: %w", err)
	}
	if n == 0 {
		return cur, terminal.ErrDrawerClosed
	}
	fresh, err := q.GetDrawerSession(ctx, row.ID)
	if err != nil {
		return cur, err
	}
	d, err := drawerOf(ctx, q, fresh, true)
	if err != nil {
		return d, err
	}
	evID := uuid.NewString()
	ev := map[string]any{"event_id": evID, "type": terminal.EventDrawerClosed, "at": c.At.UTC().Truncate(time.Millisecond), "drawer": d.Wire()}
	if err := insertSubjectEvent(ctx, q, evID, "", drawerSubject(d.ID), terminal.EventDrawerClosed, c.At, ev); err != nil {
		return d, err
	}
	journal := c.Journal
	for i := range journal {
		if len(journal[i].Detail) == 0 || string(journal[i].Detail) == "{}" {
			journal[i].Detail = jsonDetail(map[string]any{"drawer_id": d.ID, "reason": c.Reason, "expected_cents": d.ExpectedCents,
				"counted_cents": d.CountedCents, "over_short_cents": d.OverShortCents, "flagged": d.Flagged})
		}
	}
	return d, insertJournal(ctx, q, c.At, journal)
}

// CloseDrawer counts a session out. check (inside the transaction) sees the
// session before it closes (who may close it).
func (r *OrderRepository) CloseDrawer(ctx context.Context, c terminal.DrawerClose, check func(d terminal.Drawer) error) (d terminal.Drawer, err error) {
	ctx, _, end := dbSpan(ctx, "drawers.close")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		row, err := q.GetDrawerSession(ctx, c.DrawerID)
		if errors.Is(err, sql.ErrNoRows) {
			return terminal.ErrDrawerNotFound
		}
		if err != nil {
			return err
		}
		if row.ClosedAt.Valid {
			return terminal.ErrDrawerClosed
		}
		if check != nil {
			cur, err := drawerOf(ctx, q, row, false)
			if err != nil {
				return err
			}
			if err := check(cur); err != nil {
				return err
			}
		}
		d, err = closeDrawerTx(ctx, q, row, c)
		return err
	})
	return d, err
}

// HandoverDrawer counts the terminal's open drawer out (reason handover) and
// opens next on it at once, starting from the count, in one transaction.
func (r *OrderRepository) HandoverDrawer(ctx context.Context, deviceID string, c terminal.DrawerClose, next terminal.Drawer,
	journal []terminal.Entry) (closed, opened terminal.Drawer, err error) {
	ctx, _, end := dbSpan(ctx, "drawers.handover")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		row, err := q.GetOpenDrawerByDevice(ctx, deviceID)
		if errors.Is(err, sql.ErrNoRows) {
			return terminal.ErrNoDrawer
		}
		if err != nil {
			return err
		}
		if row.StaffID == next.StaffID {
			return terminal.ErrSameCashier
		}
		c.DrawerID = row.ID
		if closed, err = closeDrawerTx(ctx, q, row, c); err != nil {
			return err
		}
		next.HandoverFrom = closed.ID
		next.StartCents = *c.CountedCents
		if err := openDrawerTx(ctx, q, &next, journal, next.OpenedAt); err != nil {
			return err
		}
		opened = next
		return nil
	})
	return closed, opened, err
}

// closeOpenDrawers is Close batch's part: every open session is closed
// "closed by batch close", not counted, flagged (one drawer.closed each).
func closeOpenDrawers(ctx context.Context, q *models.Queries, at time.Time) ([]terminal.Drawer, error) {
	rows, err := q.ListOpenDrawers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]terminal.Drawer, 0, len(rows))
	for _, row := range rows {
		d, err := closeDrawerTx(ctx, q, row, terminal.DrawerClose{DrawerID: row.ID, Reason: terminal.DrawerClosedBatchClose, At: at,
			Journal: []terminal.Entry{{DeviceID: row.DeviceID, StaffID: row.StaffID, Action: terminal.ActionDrawerClose}}})
		if err != nil {
			return nil, fmt.Errorf("close drawer %s: %w", row.ID, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// drawerForPayment is the open session a cash payment taken on a terminal
// goes in ("" = no drawer open there, or not cash).
func drawerForPayment(ctx context.Context, q *models.Queries, p *terminal.Payment) (string, error) {
	if p.DeviceID == "" {
		return "", nil
	}
	row, err := q.GetOpenDrawerByDevice(ctx, p.DeviceID)
	id := row.ID
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The owner, 2026-11-12: a drawer = a terminal; its session for the
		// batch opens by itself with the terminal's first payment.
		if id, err = autoOpenDrawer(ctx, q, p); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	}
	if p.Tender != terminal.TenderCash {
		return "", nil // only cash is in a drawer
	}
	return id, nil
}

// autoOpenDrawer opens the terminal's session (start = p.DrawerStart, main's
// drawer_start_cents; auto) for the
// payment, in its transaction: drawer.opened to main + journal drawer.open.
func autoOpenDrawer(ctx context.Context, q *models.Queries, p *terminal.Payment) (string, error) {
	name := ""
	if p.DeviceID == "web" {
		name = "Pi web page"
	} else {
		if n, err := q.GetPairedDeviceName(ctx, p.DeviceID); err == nil {
			name = n
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	at := p.At
	if at.IsZero() {
		at = time.Now()
	}
	d := terminal.Drawer{ID: uuid.Must(uuid.NewV7()).String(), DeviceID: p.DeviceID, DeviceName: name, StaffID: p.StaffID,
		OpenedAt: at.UTC().Truncate(time.Millisecond), StartCents: p.DrawerStart, Auto: true}
	journal := []terminal.Entry{{StaffID: p.StaffID, DeviceID: p.DeviceID, Action: terminal.ActionDrawerOpen,
		Detail: jsonDetail(map[string]any{"drawer_id": d.ID, "start_cents": d.StartCents, "auto": true})}}
	if err := openDrawerTx(ctx, q, &d, journal, at); err != nil {
		return "", err
	}
	return d.ID, nil
}
