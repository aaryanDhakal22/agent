-- +goose Up
-- 2.2.1: "Not delivered" - a delivery that came back with its driver. Retry
-- keeps it ready with no driver, held in the "came back" list (came_back)
-- until staff release it; cancel cancels it. The last not-delivered action,
-- when it happened and the driver it came back from are kept on the order.
ALTER TABLE orders ADD COLUMN came_back boolean NOT NULL DEFAULT false;
ALTER TABLE orders ADD COLUMN not_delivered_action text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN not_delivered_at text;
ALTER TABLE orders ADD COLUMN not_delivered_driver_id text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE orders DROP COLUMN not_delivered_driver_id;
ALTER TABLE orders DROP COLUMN not_delivered_at;
ALTER TABLE orders DROP COLUMN not_delivered_action;
ALTER TABLE orders DROP COLUMN came_back;
