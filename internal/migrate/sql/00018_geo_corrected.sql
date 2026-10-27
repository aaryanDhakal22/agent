-- +goose Up
-- 2.2.2: main's order.geo "corrected" (the address was found only after
-- dropping its notes / the POS default town + zip, inside the store's map
-- box; the tablet tags it) and "note" (a short line for staff, e.g. a
-- geo.search that found nothing).
ALTER TABLE order_geo ADD COLUMN corrected boolean NOT NULL DEFAULT false;
ALTER TABLE order_geo ADD COLUMN note text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE order_geo DROP COLUMN note;
ALTER TABLE order_geo DROP COLUMN corrected;
