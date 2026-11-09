-- +goose Up
-- 2.5.0 weborder stage 3 (website / channel orders on the floor).
-- added_tip_cents: the whole tip added on the tablet after the order (ADD
-- TIP, absolute; a card channel order only) - main adds it to the card
-- capture. tip_locked: main has charged the card (the driver clocked out
-- after taking it out, or Close batch), so the tip can't change any more.
-- cancel_reason: CANCEL ORDER on a channel order (customer_asked |
-- couldnt_deliver | other); '' = not cancelled by hand.
ALTER TABLE orders ADD COLUMN added_tip_cents integer NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN tip_locked boolean NOT NULL DEFAULT false;
ALTER TABLE orders ADD COLUMN cancel_reason text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE orders DROP COLUMN cancel_reason;
ALTER TABLE orders DROP COLUMN tip_locked;
ALTER TABLE orders DROP COLUMN added_tip_cents;
