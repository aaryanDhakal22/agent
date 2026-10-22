-- +goose Up
-- 2.0.9. pool_seq: the stop order (1..n) of a delivery inside its driver's
-- pool - the AUTO plan's driving order on confirm, else the fastest order from
-- the drive times. NULL when not pooled.
ALTER TABLE orders ADD COLUMN pool_seq integer;
-- pool_plan_id: the AUTO plan whose confirm put the order in this pool (NULL
-- when pooled by hand), so a later Send Out logs the run against that plan.
-- Cleared whenever the order leaves or changes pool.
ALTER TABLE orders ADD COLUMN pool_plan_id text;

-- +goose Down
ALTER TABLE orders DROP COLUMN pool_plan_id;
ALTER TABLE orders DROP COLUMN pool_seq;
