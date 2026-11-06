-- +goose Up
-- 2.4.3 future-order actions. due_override: a future order's due time moved
-- on the Pi (CHANGE DUE TIME; RFC 3339, NULL = the vendor's promised_at).
-- made_now: MAKE NOW took it out of the future (its promised_at is ignored, it
-- prints and cooks like an order that came in now). Neither goes to main: the
-- vendor's snapshot is never rewritten.
ALTER TABLE orders ADD COLUMN due_override text;
ALTER TABLE orders ADD COLUMN made_now boolean NOT NULL DEFAULT false;
-- look: the store's colours (JSON {accent, tickets: {delivery, pickup,
-- counter}}; missing keys = defaults) for every tablet and the web UI.
ALTER TABLE settings ADD COLUMN look text NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE settings DROP COLUMN look;
ALTER TABLE orders DROP COLUMN made_now;
ALTER TABLE orders DROP COLUMN due_override;
