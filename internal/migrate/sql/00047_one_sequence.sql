-- +goose Up
-- E: ticket numbers, the owner's final rule (contract/agent.md 2.10ae,
-- 2026-11-14): one sequence for every order (settings.counter_next, from
-- main's counter_start, default 100), restarted by every Close batch. The
-- business-day columns of 00044 go. kitchen_next stays (unused, kept so an
-- older binary rolled back still starts). A future order holds a random
-- 5-digit placeholder in kitchen_number while number_placeholder = 1, until
-- it is placed and takes the next number of the sequence.
ALTER TABLE settings DROP COLUMN numbers_closed;
ALTER TABLE settings DROP COLUMN numbers_day;
ALTER TABLE orders ADD COLUMN number_placeholder boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE orders DROP COLUMN number_placeholder;
ALTER TABLE settings ADD COLUMN numbers_day text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN numbers_closed integer NOT NULL DEFAULT 0;
