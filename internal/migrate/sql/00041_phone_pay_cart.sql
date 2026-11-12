-- +goose Up
-- phone_pay_cart (contract/agent.md §2.10z, the owner 2026-11-12): a Send to
-- phone charge for a cart not sent yet. Nothing is placed or printed while it
-- waits; when the phone reports paid the Pi places the order with the
-- payment in it. order_key = the order's client_key (its SEND key),
-- order_body = the order as the terminal would SEND it (no PIN, no
-- approvals), approved_by = the manager who approved its discount / price
-- changes when the charge was made. order_id stays '' until placed.
ALTER TABLE phone_charges ADD COLUMN order_key text NOT NULL DEFAULT '';
ALTER TABLE phone_charges ADD COLUMN order_body text NOT NULL DEFAULT '';
ALTER TABLE phone_charges ADD COLUMN approved_by text NOT NULL DEFAULT '';
CREATE INDEX idx_phone_charges_order_key ON phone_charges (order_key) WHERE order_key <> '';

-- +goose Down
DROP INDEX idx_phone_charges_order_key;
ALTER TABLE phone_charges DROP COLUMN approved_by;
ALTER TABLE phone_charges DROP COLUMN order_body;
ALTER TABLE phone_charges DROP COLUMN order_key;
