-- +goose Up
-- The owner, 2026-11-12 (contract/agent.md §2.10k, §2.10n): a drawer = a
-- terminal. Its session for the batch opens by itself with the terminal's
-- first payment (auto), so sessions are per terminal, not per cashier: the
-- one-open-session-per-cashier index goes. A refund says what kind it is
-- ('' plain | goodwill | outside: refunded in Square's dashboard).
DROP INDEX IF EXISTS idx_drawer_sessions_open_staff;
ALTER TABLE drawer_sessions ADD COLUMN auto boolean NOT NULL DEFAULT false;
ALTER TABLE order_payments ADD COLUMN refund_kind text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE order_payments DROP COLUMN refund_kind;
ALTER TABLE drawer_sessions DROP COLUMN auto;
CREATE UNIQUE INDEX idx_drawer_sessions_open_staff ON drawer_sessions (staff_id) WHERE closed_at IS NULL;
