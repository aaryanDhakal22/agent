-- +goose Up
-- An accept is claimed (state 'accepted', kitchen number) before its tickets
-- print and is recorded (floor event) after. accept_pending marks the gap: a
-- crash or power cut inside it leaves the flag set, and the next start puts
-- the order back to 'arrived' so it rings again instead of silently sitting
-- in the kitchen list unprinted.
ALTER TABLE orders ADD COLUMN accept_pending integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE orders DROP COLUMN accept_pending;
