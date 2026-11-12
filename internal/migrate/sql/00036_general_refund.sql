-- +goose Up
-- General refund (the owner, 2026-11-12; contract/agent.md §2.10x): cash given
-- back at the counter with no order. It is an order_payments row with
-- order_id '' and refund_kind 'general' (so the drawer, the till report and the
-- Z count it like any other cash refund). The S-6 trigger 00033 refuses a
-- payment whose order doesn't exist: it now lets exactly that row through
-- (a refund, kind general, no order) and still refuses every other one.
DROP TRIGGER IF EXISTS order_payments_order_ins;
-- +goose StatementBegin
CREATE TRIGGER order_payments_order_ins BEFORE INSERT ON order_payments
WHEN NOT (NEW.order_id = '' AND NEW.refund AND NEW.refund_kind = 'general')
  AND NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id)
BEGIN
  SELECT RAISE(ABORT, 'order_payments: no such order');
END;
-- +goose StatementEnd

-- +goose Down
DELETE FROM order_payments WHERE order_id = '' AND refund_kind = 'general';
DROP TRIGGER IF EXISTS order_payments_order_ins;
-- +goose StatementBegin
CREATE TRIGGER order_payments_order_ins BEFORE INSERT ON order_payments
WHEN NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id)
BEGIN
  SELECT RAISE(ABORT, 'order_payments: no such order');
END;
-- +goose StatementEnd
