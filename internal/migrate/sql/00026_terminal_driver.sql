-- +goose Up
-- The terminal's driver view + the batch auto-close (contract/agent.md
-- §2.10b, §2.10c).
--
-- terminal_driver_links: a driver staff member (main's staff id) and the Pi
-- driver they are on the roster as (linked at their first login).
CREATE TABLE terminal_driver_links (
    staff_id  text PRIMARY KEY,
    driver_id text NOT NULL UNIQUE,
    linked_at text NOT NULL
);

-- door_tip_cents: a tip a driver got at the door on a cash or counter
-- delivery (absolute, cents). The driver keeps it; his report shows it.
ALTER TABLE orders ADD COLUMN door_tip_cents integer NOT NULL DEFAULT 0;

-- auto_close_last: the scheduled time (RFC 3339, UTC) of the last batch
-- auto-close slot the Pi handled ('' = none yet), so a restart neither
-- repeats nor skips one.
ALTER TABLE settings ADD COLUMN auto_close_last text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE settings DROP COLUMN auto_close_last;
ALTER TABLE orders DROP COLUMN door_tip_cents;
DROP TABLE terminal_driver_links;
