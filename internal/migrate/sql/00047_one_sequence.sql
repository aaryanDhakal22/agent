-- +goose Up
-- E: ticket numbers, the owner's final rule (contract/agent.md 2.10ae,
-- 2026-11-14): one sequence for every order (settings.counter_next, from
-- main's counter_start, default 100), restarted by every Close batch. The
-- business-day columns of 00044 go. kitchen_next stays (unused, kept so an
-- older binary rolled back still starts). A future order holds a random
-- 5-digit placeholder in kitchen_number while number_placeholder = 1, until
-- it is placed and takes the next number of the sequence.
-- A Close batch that ran on the 2026-11-13 binary only marked the numbers
-- closed (numbers_closed = 1, restart at the next business day's first
-- number). Honour it before the column goes, or the sequence carries on
-- from where it was (Lombardi's 2026-11-14: closed on 2.5.8, updated to
-- 2.5.9 by the round right after, numbers went on at 170 instead of 100).
UPDATE settings SET counter_next = 0, kitchen_next = 0 WHERE numbers_closed <> 0;
ALTER TABLE settings DROP COLUMN numbers_closed;
ALTER TABLE settings DROP COLUMN numbers_day;
ALTER TABLE orders ADD COLUMN number_placeholder boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE orders DROP COLUMN number_placeholder;
ALTER TABLE settings ADD COLUMN numbers_day text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN numbers_closed integer NOT NULL DEFAULT 0;
