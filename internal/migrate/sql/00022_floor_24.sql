-- +goose Up
-- 2.4.0 floor release. marked_delivered: a delivery marked delivered with no
-- driver (MARK DELIVERED on the tablet, or moved to Unassigned) - JSON {at,
-- by_kind, by_name, device_id, prev_state, available}; '' = never. It stays
-- after the order is assigned to a driver (the record of how it got there).
-- paid_at: when a POS check last went from a balance due to paid ($0), so a
-- paid takeout's wait clock starts at the payment.
ALTER TABLE orders ADD COLUMN marked_delivered text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN paid_at text;
-- early_kitchen / early_receipt: a future order's kitchen tickets / customer
-- receipt printed at its first ACCEPT (the store's future switches) - its
-- CONFIRM then doesn't print the receipt again.
ALTER TABLE orders ADD COLUMN early_kitchen boolean NOT NULL DEFAULT false;
ALTER TABLE orders ADD COLUMN early_receipt boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE orders DROP COLUMN early_receipt;
ALTER TABLE orders DROP COLUMN early_kitchen;
ALTER TABLE orders DROP COLUMN paid_at;
ALTER TABLE orders DROP COLUMN marked_delivered;
