-- +goose Up
-- Counter/takeout on Kitchen, terminal changes, finishing orders, the order
-- callout. finished_at / finished_by: a ready pickup or counter order that is
-- done with (picked up) - 'pos' when the store's POS closed the check, 'auto'
-- when an online pickup sat ready for the store's finish time. It no longer
-- counts as open for a forced Pi update. pos_closed: the POS closed the check
-- (a closed check still cooking finishes when it is marked ready).
-- pos_change: the last terminal change to a check already on the tablet that
-- nobody acknowledged yet (JSON; '' = none).
ALTER TABLE orders ADD COLUMN finished_at text;
ALTER TABLE orders ADD COLUMN finished_by text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN pos_closed boolean NOT NULL DEFAULT false;
ALTER TABLE orders ADD COLUMN pos_change text NOT NULL DEFAULT '';
-- finishing: {online_pickup_minutes}; callout: the store hub's order callout
-- settings (JSON over the defaults).
ALTER TABLE settings ADD COLUMN finishing text NOT NULL DEFAULT '{}';
ALTER TABLE settings ADD COLUMN callout text NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE settings DROP COLUMN callout;
ALTER TABLE settings DROP COLUMN finishing;
ALTER TABLE orders DROP COLUMN pos_change;
ALTER TABLE orders DROP COLUMN pos_closed;
ALTER TABLE orders DROP COLUMN finished_by;
ALTER TABLE orders DROP COLUMN finished_at;
