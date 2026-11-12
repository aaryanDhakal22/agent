-- +goose Up
-- H3 (HOUSEKEEPING-PLAN.md "H3 - agent part"): SoftTouch is gone. The store hub
-- stays as the callout box (pairing, its status, the feed, callout audio); the
-- POS checks it pulled, the POS driver numbers and every POS-only column go.
--
-- 1. The POS checks the Pi still holds (local only, never synced to main;
--    their customer data was wiped at Close batch) and their side rows.
DELETE FROM order_geo WHERE order_id IN (SELECT id FROM orders WHERE ext_source <> '');
DELETE FROM order_times WHERE a IN (SELECT id FROM orders WHERE ext_source <> '')
   OR b IN (SELECT id FROM orders WHERE ext_source <> '');
DELETE FROM floor_events WHERE order_id IN (SELECT id FROM orders WHERE ext_source <> '');
DELETE FROM orders WHERE ext_source <> '';

-- 2. The POS columns of orders (00014_integration, 00019_callout).
DROP INDEX idx_orders_ext;
ALTER TABLE orders DROP COLUMN ext_source;
ALTER TABLE orders DROP COLUMN ext_ref;
ALTER TABLE orders DROP COLUMN ext_hash;
ALTER TABLE orders DROP COLUMN ext_driver_pos;
ALTER TABLE orders DROP COLUMN display_number;
ALTER TABLE orders DROP COLUMN board_hidden;
ALTER TABLE orders DROP COLUMN local_only;
ALTER TABLE orders DROP COLUMN flag;
ALTER TABLE orders DROP COLUMN pos_check_number;
ALTER TABLE orders DROP COLUMN pos_driver_note;
ALTER TABLE orders DROP COLUMN pos_closed;
ALTER TABLE orders DROP COLUMN pos_change;

-- 3. The drivers' POS login number (00014).
DROP INDEX idx_drivers_pos_number;
ALTER TABLE drivers DROP COLUMN pos_number;

-- 4. The POS feature and ticket switches in the settings (the rest stays:
--    future_orders, paid_wave, the online / future ticket switches).
UPDATE settings SET features = json_remove(features, '$.pos_pull', '$.order_insert', '$.driver_writeback', '$.address_heal')
WHERE json_valid(features);
UPDATE settings SET ticket_toggles = json_remove(ticket_toggles, '$.pos_delivery', '$.pos_takeout', '$.pos_counter')
WHERE json_valid(ticket_toggles);

-- +goose Down
-- The columns come back empty (the POS checks and numbers are not restored).
ALTER TABLE drivers ADD COLUMN pos_number text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_drivers_pos_number ON drivers (pos_number) WHERE pos_number <> '' AND active;
ALTER TABLE orders ADD COLUMN pos_change text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN pos_closed boolean NOT NULL DEFAULT false;
ALTER TABLE orders ADD COLUMN pos_driver_note text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN pos_check_number integer;
ALTER TABLE orders ADD COLUMN flag text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN local_only boolean NOT NULL DEFAULT false;
ALTER TABLE orders ADD COLUMN board_hidden boolean NOT NULL DEFAULT false;
ALTER TABLE orders ADD COLUMN display_number integer;
ALTER TABLE orders ADD COLUMN ext_driver_pos text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN ext_hash text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN ext_ref text NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN ext_source text NOT NULL DEFAULT '';
CREATE INDEX idx_orders_ext ON orders (ext_source, ext_ref) WHERE ext_ref <> '';
