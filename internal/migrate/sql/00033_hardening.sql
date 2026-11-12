-- +goose Up
-- Hardening round (2026-11-12, HARDENING-PLAN.md batch A1; contract/agent.md §2.10s).
--
-- M-1 / M-8: a keyed request's fingerprint (sha256 of the request without
-- client_key / manager_pin): the same key with another body is 409 key_reused.
ALTER TABLE orders ADD COLUMN request_hash text NOT NULL DEFAULT '';
ALTER TABLE order_payments ADD COLUMN request_hash text NOT NULL DEFAULT '';
-- M-10: an unsettled check carried past its batch's close (still payable,
-- on the Pickup list) until it is settled.
ALTER TABLE orders ADD COLUMN carried boolean NOT NULL DEFAULT false;
-- M-11: a counter order's kitchen tickets are still to print (set in the
-- order's transaction, cleared after the print; reprinted at start).
ALTER TABLE orders ADD COLUMN kitchen_pending boolean NOT NULL DEFAULT false;
-- M-10: the batch a payment was taken in (a carried order's later payment
-- counts on the batch it is taken in). Backfilled from the order.
ALTER TABLE order_payments ADD COLUMN batch_id integer;
UPDATE order_payments SET batch_id = (SELECT o.batch_id FROM orders o WHERE o.id = order_payments.order_id);
CREATE INDEX idx_order_payments_batch ON order_payments (batch_id);
CREATE INDEX idx_orders_carried ON orders (carried) WHERE carried;
-- M-9: a Z report that doesn't balance is flagged (paged once).
ALTER TABLE z_reports ADD COLUMN flagged boolean NOT NULL DEFAULT false;
-- X-2: wrong PINs per terminal and per store, in a window a right PIN
-- doesn't reset; kept across restarts (JSON, application/terminal staff.go).
ALTER TABLE settings ADD COLUMN pin_throttle text NOT NULL DEFAULT '';

-- S-6: money and batch rules the database keeps, not only Go. SQLite can't
-- add a CHECK or a FOREIGN KEY to an existing table without rebuilding it,
-- and a rebuild (or a unique index) would fail the migration - and the Pi's
-- start - on an old row that breaks a rule. So these are triggers on new
-- writes: an old row is left as it is, every new one is checked.
-- +goose StatementBegin
CREATE TRIGGER order_payments_money_ins BEFORE INSERT ON order_payments
WHEN NEW.amount_cents < 0 OR NEW.tip_cents < 0 OR NEW.change_cents < 0 OR NEW.tendered_cents < 0
  OR NEW.tender NOT IN ('cash', 'card', 'square_app', 'manual_card', 'comp')
  OR (NEW.amount_cents = 0 AND NEW.tip_cents = 0)
BEGIN
  SELECT RAISE(ABORT, 'order_payments: bad money');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER order_payments_order_ins BEFORE INSERT ON order_payments
WHEN NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id)
BEGIN
  SELECT RAISE(ABORT, 'order_payments: no such order');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER order_payments_money_upd BEFORE UPDATE OF amount_cents, tip_cents, change_cents, tender ON order_payments
WHEN NEW.amount_cents < 0 OR NEW.tip_cents < 0 OR NEW.change_cents < 0
  OR NEW.tender NOT IN ('cash', 'card', 'square_app', 'manual_card', 'comp')
BEGIN
  SELECT RAISE(ABORT, 'order_payments: bad money');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER drawer_sessions_money_ins BEFORE INSERT ON drawer_sessions
WHEN NEW.start_cents < 0
BEGIN
  SELECT RAISE(ABORT, 'drawer_sessions: bad start');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER drawer_movements_ins BEFORE INSERT ON drawer_movements
WHEN NEW.amount_cents <= 0 OR NOT EXISTS (SELECT 1 FROM drawer_sessions WHERE id = NEW.drawer_id)
BEGIN
  SELECT RAISE(ABORT, 'drawer_movements: bad amount or no such drawer');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER batches_one_open BEFORE INSERT ON batches
WHEN NEW.closed_at IS NULL AND EXISTS (SELECT 1 FROM batches WHERE closed_at IS NULL)
BEGIN
  SELECT RAISE(ABORT, 'batches: a batch is open already');
END;
-- +goose StatementEnd
-- A unique (batch, kitchen number) rule is left out on purpose: a future
-- order moved into the next batch keeps its number while the new batch's
-- numbers start again, and an abort there would fail an accept mid-rush.

-- +goose Down
DROP TRIGGER IF EXISTS batches_one_open;
DROP TRIGGER IF EXISTS drawer_movements_ins;
DROP TRIGGER IF EXISTS drawer_sessions_money_ins;
DROP TRIGGER IF EXISTS order_payments_money_upd;
DROP TRIGGER IF EXISTS order_payments_order_ins;
DROP TRIGGER IF EXISTS order_payments_money_ins;
ALTER TABLE settings DROP COLUMN pin_throttle;
ALTER TABLE z_reports DROP COLUMN flagged;
DROP INDEX IF EXISTS idx_orders_carried;
DROP INDEX IF EXISTS idx_order_payments_batch;
ALTER TABLE order_payments DROP COLUMN batch_id;
ALTER TABLE orders DROP COLUMN kitchen_pending;
ALTER TABLE orders DROP COLUMN carried;
ALTER TABLE order_payments DROP COLUMN request_hash;
ALTER TABLE orders DROP COLUMN request_hash;
