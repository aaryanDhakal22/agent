-- +goose Up
-- Terminal stage 2 (contract/agent.md §2.4e, §2.10k, §2.10l).
--
-- 1. Floor sync dead letters. An event main refuses for good that the Pi
--    keeps (floorsync.KeepRefused) is retried with the backoff; after
--    settings.sync_park_refusals refusals or settings.sync_park_hours since
--    the first one it is PARKED: floor_events.parked_at is set (the row keeps
--    its place in the queue, so a retry puts it back where it was) and a
--    floor_dead_letters row records why. Later events about the same subject
--    (floor_events.subject: an order id, "customer:<id>", "drawer:<id>") wait
--    behind it; the rest of the queue moves on.
ALTER TABLE floor_events ADD COLUMN subject text NOT NULL DEFAULT '';
ALTER TABLE floor_events ADD COLUMN refusals integer NOT NULL DEFAULT 0;
ALTER TABLE floor_events ADD COLUMN first_refused_at text;
ALTER TABLE floor_events ADD COLUMN parked_at text;
UPDATE floor_events SET subject = COALESCE(order_id, '');
CREATE INDEX idx_floor_events_parked ON floor_events (subject) WHERE parked_at IS NOT NULL AND synced_at IS NULL;

CREATE TABLE floor_dead_letters (
    event_id         text    PRIMARY KEY,
    subject          text    NOT NULL DEFAULT '',
    type             text    NOT NULL,
    result           text    NOT NULL DEFAULT '',
    refusals         integer NOT NULL DEFAULT 0,
    first_refused_at text    NOT NULL DEFAULT '',
    parked_at        text    NOT NULL,
    last_error       text    NOT NULL DEFAULT '',
    retried_at       text,
    retried_by       text    NOT NULL DEFAULT ''
);
CREATE INDEX idx_floor_dead_letters_open ON floor_dead_letters (parked_at) WHERE retried_at IS NULL;

-- The parking thresholds (Pi settings, PUT /api/settings/sync) and the last
-- parked-events Pushover (at most one an hour).
ALTER TABLE settings ADD COLUMN sync_park_refusals integer NOT NULL DEFAULT 8;
ALTER TABLE settings ADD COLUMN sync_park_hours integer NOT NULL DEFAULT 2;
ALTER TABLE settings ADD COLUMN sync_park_alerted_at text NOT NULL DEFAULT '';

-- 2. Cash drawers: one session per terminal (its drawer) and cashier shift.
--    device_id = the terminal's paired device ("web" = the Pi page).
--    counted_cents NULL = never counted (closed by Close batch: flagged).
CREATE TABLE drawer_sessions (
    id                 text    PRIMARY KEY,
    device_id          text    NOT NULL,
    device_name        text    NOT NULL DEFAULT '',
    staff_id           text    NOT NULL,
    staff_name         text    NOT NULL DEFAULT '',
    batch_id           integer NOT NULL,
    opened_at          text    NOT NULL,
    start_cents        integer NOT NULL,
    handover_from      text    NOT NULL DEFAULT '',
    closed_at          text,
    closed_by          text    NOT NULL DEFAULT '',
    close_reason       text    NOT NULL DEFAULT '',
    counted_cents      integer,
    denominations      text    NOT NULL DEFAULT '',
    expected_cents     integer,
    flagged            boolean NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX idx_drawer_sessions_open_device ON drawer_sessions (device_id) WHERE closed_at IS NULL;
CREATE UNIQUE INDEX idx_drawer_sessions_open_staff ON drawer_sessions (staff_id) WHERE closed_at IS NULL;
CREATE INDEX idx_drawer_sessions_batch ON drawer_sessions (batch_id);

-- Pay-ins and pay-outs (cash put in / taken out without a sale).
CREATE TABLE drawer_movements (
    id           text    PRIMARY KEY,
    drawer_id    text    NOT NULL,
    kind         text    NOT NULL CHECK (kind IN ('pay_in', 'pay_out')),
    amount_cents integer NOT NULL,
    reason       text    NOT NULL DEFAULT '',
    staff_id     text    NOT NULL DEFAULT '',
    approved_by  text    NOT NULL DEFAULT '',
    client_key   text    NOT NULL DEFAULT '',
    at           text    NOT NULL
);
CREATE INDEX idx_drawer_movements_drawer ON drawer_movements (drawer_id, at);
CREATE UNIQUE INDEX idx_drawer_movements_key ON drawer_movements (drawer_id, client_key) WHERE client_key <> '';

-- A cash payment (or refund) taken on a terminal goes in that terminal's
-- open drawer ('' = no drawer open: counted outside the drawers).
ALTER TABLE order_payments ADD COLUMN drawer_id text NOT NULL DEFAULT '';
CREATE INDEX idx_order_payments_drawer ON order_payments (drawer_id) WHERE drawer_id <> '';

-- 3. The Z report: one per closed batch. status 'pending' is written in
--    Close batch's transaction; the report is built right after (and at
--    start for one a crash left pending), then sent as z.report.
CREATE TABLE z_reports (
    batch_id   integer PRIMARY KEY,
    id         text    NOT NULL,
    status     text    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done')),
    close_kind text    NOT NULL DEFAULT 'manual',
    forced     boolean NOT NULL DEFAULT false,
    report     text    NOT NULL DEFAULT '',
    created_at text    NOT NULL,
    built_at   text
);

-- +goose Down
DROP TABLE z_reports;
DROP INDEX idx_order_payments_drawer;
ALTER TABLE order_payments DROP COLUMN drawer_id;
DROP TABLE drawer_movements;
DROP TABLE drawer_sessions;
ALTER TABLE settings DROP COLUMN sync_park_alerted_at;
ALTER TABLE settings DROP COLUMN sync_park_hours;
ALTER TABLE settings DROP COLUMN sync_park_refusals;
DROP TABLE floor_dead_letters;
DROP INDEX idx_floor_events_parked;
ALTER TABLE floor_events DROP COLUMN parked_at;
ALTER TABLE floor_events DROP COLUMN first_refused_at;
ALTER TABLE floor_events DROP COLUMN refusals;
ALTER TABLE floor_events DROP COLUMN subject;
