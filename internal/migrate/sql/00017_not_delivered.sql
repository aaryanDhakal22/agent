-- +goose Up
-- 2.2.1: "Not delivered" - a delivery that came back with its driver. Retry
-- keeps it ready with no driver, held in the "came back" list (came_back)
-- until staff release it; cancel cancels it. The last not-delivered action,
-- when it happened and the driver it came back from are kept on the order.
-- run_seq is a sent order's stop number on its driver's run (1..n, the
-- delivery.run stop order; a stop joining the run goes last).
ALTER TABLE orders ADD COLUMN came_back boolean NOT NULL DEFAULT false;
ALTER TABLE orders ADD COLUMN not_delivered_action text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN not_delivered_at text;
ALTER TABLE orders ADD COLUMN not_delivered_driver_id text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN run_seq integer;
-- Simple auto-assign: the next available driver with an empty pool gets
-- the ready deliveries the planner gives him (pooled, not sent).
ALTER TABLE settings ADD COLUMN auto_assign boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE settings DROP COLUMN auto_assign;
ALTER TABLE orders DROP COLUMN run_seq;
ALTER TABLE orders DROP COLUMN not_delivered_driver_id;
ALTER TABLE orders DROP COLUMN not_delivered_at;
ALTER TABLE orders DROP COLUMN not_delivered_action;
ALTER TABLE orders DROP COLUMN came_back;
