-- +goose Up
-- +goose StatementBegin
-- Drivers work like the old tablet app (owner decision 2026-10-16): a driver
-- clocks in to join the queue and clocks out to leave it; every shift is kept
-- as a record for reports (not wages); everyone still clocked in is clocked
-- out at a daily reset time. The drivers table is rebuilt to add the 'off'
-- status (SQLite can't widen a CHECK); existing drivers keep their status.
CREATE TABLE drivers_new (
    id               text    PRIMARY KEY,
    name             text    NOT NULL,
    active           boolean NOT NULL DEFAULT true,
    position         integer NOT NULL DEFAULT 0,
    status           text    NOT NULL DEFAULT 'off',
    last_returned_at text,
    created_at       text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (status IN ('off', 'available', 'out'))
);
INSERT INTO drivers_new (id, name, active, position, status, last_returned_at, created_at)
SELECT id, name, active, position, status, last_returned_at, created_at FROM drivers;
DROP TABLE drivers;
ALTER TABLE drivers_new RENAME TO drivers;

CREATE TABLE driver_shifts (
    id          text    PRIMARY KEY,
    driver_id   text    NOT NULL,
    driver_name text    NOT NULL,
    batch_id    integer NOT NULL,
    clock_in    text    NOT NULL,
    clock_out   text,
    auto_out    boolean NOT NULL DEFAULT false
);
CREATE INDEX idx_driver_shifts_open ON driver_shifts (driver_id) WHERE clock_out IS NULL;

-- Daily reset (location-local HH:MM) + when it last ran.
ALTER TABLE settings ADD COLUMN driver_reset_time text NOT NULL DEFAULT '10:00';
ALTER TABLE settings ADD COLUMN last_driver_reset_at text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE settings DROP COLUMN last_driver_reset_at;
ALTER TABLE settings DROP COLUMN driver_reset_time;
DROP TABLE driver_shifts;
CREATE TABLE drivers_old (
    id               text    PRIMARY KEY,
    name             text    NOT NULL,
    active           boolean NOT NULL DEFAULT true,
    position         integer NOT NULL DEFAULT 0,
    status           text    NOT NULL DEFAULT 'available',
    last_returned_at text,
    created_at       text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (status IN ('available', 'out'))
);
INSERT INTO drivers_old (id, name, active, position, status, last_returned_at, created_at)
SELECT id, name, active, position, CASE WHEN status = 'off' THEN 'available' ELSE status END, last_returned_at, created_at FROM drivers;
DROP TABLE drivers;
ALTER TABLE drivers_old RENAME TO drivers;
-- +goose StatementEnd
