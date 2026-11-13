-- +goose Up
-- Ticket numbers restart once a business day (contract/agent.md §2.10ae, the
-- owner 2026-11-13): Close batch no longer resets them; it marks the numbers
-- closed, and the first number taken on a new business day after a close
-- starts both sequences again (kitchen at main's kitchen_start, counter at
-- counter_start). numbers_day = the business day ('YYYY-MM-DD', store time,
-- the day starting at the auto-close time) of the last number taken ('' =
-- none yet); numbers_closed = a close happened since that number.
-- kitchen_next now reads like counter_next: 0 = start again.
ALTER TABLE settings ADD COLUMN numbers_day text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN numbers_closed integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE settings DROP COLUMN numbers_closed;
ALTER TABLE settings DROP COLUMN numbers_day;
