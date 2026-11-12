package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"
)

// payment_cards (migration 00040, contract 2.10r): Square's answer for an
// in-app card payment, or for a refund given through Square, kept as JSON
// beside its order_payments row (one table of its own, so sqlc's SELECT * on
// order_payments stays as it is). Hand-written SQL through Queries.Raw.

// saveCard stores a payment's card info (nothing when it has none), in the
// payment's transaction.
func saveCard(ctx context.Context, q *models.Queries, p *terminal.Payment) error {
	if p.CardInfo.Empty() {
		return nil
	}
	raw, err := json.Marshal(p.CardInfo)
	if err != nil {
		return err
	}
	if _, err := q.Raw().ExecContext(ctx, `INSERT INTO payment_cards (payment_id, card) VALUES (?, ?)
		ON CONFLICT (payment_id) DO UPDATE SET card = excluded.card`, p.ID, string(raw)); err != nil {
		return fmt.Errorf("insert payment card: %w", err)
	}
	return nil
}

// cardOf fills one payment's card info.
func cardOf(ctx context.Context, q *models.Queries, p *terminal.Payment) error {
	var raw string
	err := q.Raw().QueryRowContext(ctx, `SELECT card FROM payment_cards WHERE payment_id = ?`, p.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), &p.CardInfo)
}

// cardsOfOrder fills the card info of an order's payments.
func cardsOfOrder(ctx context.Context, q *models.Queries, orderID string, pays []terminal.Payment) error {
	if len(pays) == 0 {
		return nil
	}
	rows, err := q.Raw().QueryContext(ctx, `SELECT c.payment_id, c.card FROM payment_cards c
		JOIN order_payments p ON p.id = c.payment_id WHERE p.order_id = ?`, orderID)
	if err != nil {
		return err
	}
	defer rows.Close()
	at := make(map[string]int, len(pays))
	for i := range pays {
		at[pays[i].ID] = i
	}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		if i, ok := at[id]; ok {
			if err := json.Unmarshal([]byte(raw), &pays[i].CardInfo); err != nil {
				return err
			}
		}
	}
	return rows.Err()
}

// deleteOrphanCards drops card rows whose payment is gone (retention
// deleted its order), in TrimBatches' transaction.
func deleteOrphanCards(ctx context.Context, q *models.Queries) error {
	_, err := q.Raw().ExecContext(ctx, `DELETE FROM payment_cards WHERE payment_id NOT IN (SELECT id FROM order_payments)`)
	return err
}
