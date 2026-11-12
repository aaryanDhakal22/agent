package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/phonepay"
)

// PhoneChargeRepository keeps "Send to phone" charges (contract §2.10z,
// migration 00039_phone_pay). Plain database/sql (no sqlc): one small table.
type PhoneChargeRepository struct{ db *sql.DB }

var _ phonepay.Repo = (*PhoneChargeRepository)(nil)

func NewPhoneChargeRepository(db *sql.DB) *PhoneChargeRepository {
	return &PhoneChargeRepository{db: db}
}

const phoneChargeCols = `id, client_key, request_hash, order_id, number, phone_id, phone_name, device_id, device_name,
staff_id, staff_name, amount_cents, tip_cents, code, note, status, message, created_at, updated_at, expires_at,
opened_at, finished_at, transaction_id, client_transaction_id, error_code, payment_id, square_payment_id, brand,
last4, entry, extra_tip_cents, order_key, order_body, approved_by`

func phoneTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTS(*t)
}

func phoneChargeArgs(c phonepay.Charge) []any {
	return []any{c.ID, c.ClientKey, c.RequestHash, c.OrderID, c.Number, c.PhoneID, c.PhoneName, c.DeviceID, c.DeviceName,
		c.StaffID, c.StaffName, c.AmountCents, c.TipCents, c.Code, c.Note, c.Status, c.Message, fmtTS(c.CreatedAt),
		fmtTS(c.UpdatedAt), fmtTS(c.ExpiresAt), phoneTS(c.OpenedAt), phoneTS(c.FinishedAt), c.TransactionID,
		c.ClientTransactionID, c.ErrorCode, c.PaymentID, c.SquarePaymentID, c.Brand, c.Last4, c.Entry, c.ExtraTipCents,
		c.OrderKey, string(c.OrderBody), c.ApprovedBy}
}

type rowScanner interface{ Scan(dest ...any) error }

func scanPhoneCharge(r rowScanner) (phonepay.Charge, error) {
	var c phonepay.Charge
	var created, updated, expires string
	var opened, finished sql.NullString
	var body string
	err := r.Scan(&c.ID, &c.ClientKey, &c.RequestHash, &c.OrderID, &c.Number, &c.PhoneID, &c.PhoneName, &c.DeviceID,
		&c.DeviceName, &c.StaffID, &c.StaffName, &c.AmountCents, &c.TipCents, &c.Code, &c.Note, &c.Status, &c.Message,
		&created, &updated, &expires, &opened, &finished, &c.TransactionID, &c.ClientTransactionID, &c.ErrorCode,
		&c.PaymentID, &c.SquarePaymentID, &c.Brand, &c.Last4, &c.Entry, &c.ExtraTipCents, &c.OrderKey, &body, &c.ApprovedBy)
	if err != nil {
		return c, err
	}
	if body != "" {
		c.OrderBody = json.RawMessage(body)
	}
	if c.CreatedAt, err = parseTS(created); err != nil {
		return c, err
	}
	if c.UpdatedAt, err = parseTS(updated); err != nil {
		return c, err
	}
	if c.ExpiresAt, err = parseTS(expires); err != nil {
		return c, err
	}
	for _, p := range []struct {
		s   sql.NullString
		dst **time.Time
	}{{opened, &c.OpenedAt}, {finished, &c.FinishedAt}} {
		if p.s.Valid && p.s.String != "" {
			t, err := parseTS(p.s.String)
			if err != nil {
				return c, err
			}
			*p.dst = &t
		}
	}
	return c, nil
}

func (r *PhoneChargeRepository) Create(ctx context.Context, c phonepay.Charge) (got phonepay.Charge, dup bool, err error) {
	ctx, _, end := dbSpan(ctx, "phone_charges.create")
	defer end(&err)
	ph := strings.TrimSuffix(strings.Repeat("?,", 34), ",")
	res, err := r.db.ExecContext(ctx, `INSERT INTO phone_charges (`+phoneChargeCols+`) VALUES (`+ph+`)
ON CONFLICT (client_key) DO NOTHING`, phoneChargeArgs(c)...)
	if err != nil {
		return got, false, fmt.Errorf("insert phone charge: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return c, false, nil
	}
	got, err = scanPhoneCharge(r.db.QueryRowContext(ctx, `SELECT `+phoneChargeCols+` FROM phone_charges WHERE client_key = ?`, c.ClientKey))
	if err != nil {
		return got, false, fmt.Errorf("read phone charge by key: %w", err)
	}
	return got, true, nil
}

func (r *PhoneChargeRepository) Get(ctx context.Context, id string) (c phonepay.Charge, err error) {
	ctx, _, end := dbSpan(ctx, "phone_charges.get")
	defer end(&err)
	c, err = scanPhoneCharge(r.db.QueryRowContext(ctx, `SELECT `+phoneChargeCols+` FROM phone_charges WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, phonepay.ErrNotFound
	}
	return c, err
}

func (r *PhoneChargeRepository) ByKey(ctx context.Context, key string) (c phonepay.Charge, err error) {
	ctx, _, end := dbSpan(ctx, "phone_charges.by_key")
	defer end(&err)
	c, err = scanPhoneCharge(r.db.QueryRowContext(ctx, `SELECT `+phoneChargeCols+` FROM phone_charges WHERE client_key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return c, phonepay.ErrNotFound
	}
	return c, err
}

func (r *PhoneChargeRepository) Update(ctx context.Context, c phonepay.Charge) (err error) {
	ctx, _, end := dbSpan(ctx, "phone_charges.update")
	defer end(&err)
	_, err = r.db.ExecContext(ctx, `UPDATE phone_charges SET status = ?, message = ?, updated_at = ?, opened_at = ?,
finished_at = ?, transaction_id = ?, client_transaction_id = ?, error_code = ?, payment_id = ?, square_payment_id = ?,
brand = ?, last4 = ?, entry = ?, extra_tip_cents = ?, order_id = ?, number = ? WHERE id = ?`,
		c.Status, c.Message, fmtTS(c.UpdatedAt), phoneTS(c.OpenedAt), phoneTS(c.FinishedAt), c.TransactionID,
		c.ClientTransactionID, c.ErrorCode, c.PaymentID, c.SquarePaymentID, c.Brand, c.Last4, c.Entry, c.ExtraTipCents, c.OrderID, c.Number, c.ID)
	return err
}

func (r *PhoneChargeRepository) list(ctx context.Context, q string, args ...any) ([]phonepay.Charge, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+phoneChargeCols+` FROM phone_charges `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []phonepay.Charge{}
	for rows.Next() {
		c, err := scanPhoneCharge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *PhoneChargeRepository) OpenFor(ctx context.Context, phoneID string) (out []phonepay.Charge, err error) {
	ctx, _, end := dbSpan(ctx, "phone_charges.open")
	defer end(&err)
	if phoneID == "" {
		return r.list(ctx, `WHERE status IN ('pending','in_app','unrecorded') ORDER BY created_at`)
	}
	return r.list(ctx, `WHERE phone_id = ? AND status IN ('pending','in_app','unrecorded') ORDER BY created_at`, phoneID)
}

func (r *PhoneChargeRepository) ForOrder(ctx context.Context, orderID string) (out []phonepay.Charge, err error) {
	ctx, _, end := dbSpan(ctx, "phone_charges.for_order")
	defer end(&err)
	return r.list(ctx, `WHERE order_id = ? ORDER BY created_at DESC`, orderID)
}

func (r *PhoneChargeRepository) Recent(ctx context.Context, limit int) (out []phonepay.Charge, err error) {
	ctx, _, end := dbSpan(ctx, "phone_charges.recent")
	defer end(&err)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return r.list(ctx, `ORDER BY created_at DESC LIMIT ?`, limit)
}

// TrimPhoneCharges deletes finished charges older than before (retention).
func (r *PhoneChargeRepository) Trim(ctx context.Context, before time.Time) (n int64, err error) {
	ctx, _, end := dbSpan(ctx, "phone_charges.trim")
	defer end(&err)
	res, err := r.db.ExecContext(ctx, `DELETE FROM phone_charges WHERE created_at < ? AND status IN ('paid','declined','cancelled','expired')`, fmtTS(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
