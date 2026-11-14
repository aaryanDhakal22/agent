-- +goose Up
-- Card tips the driver types before Close batch (contract/agent.md §2.10am,
-- batch F, the owner 2026-11-14): one row per tippable card delivery once a
-- tip state was saved. state: tipped | no_tip | none (set back by a
-- manager). tip_cents = the typed tip (tipped only); applied_cents = what
-- the press of Close batch has put on the order so far (the next press puts
-- the difference). square_state: '' | sent | adjusted | not_adjustable |
-- failed (the Square tip adjustment of a counter card payment).
CREATE TABLE driver_tips (
    order_id          text    PRIMARY KEY,
    batch_id          integer NOT NULL,
    driver_id         text    NOT NULL DEFAULT '',
    state             text    NOT NULL,
    tip_cents         integer NOT NULL DEFAULT 0,
    applied_cents     integer NOT NULL DEFAULT 0,
    set_at            text    NOT NULL,
    set_by            text    NOT NULL DEFAULT '',
    applied_at        text,
    square_state      text    NOT NULL DEFAULT '',
    square_error      text    NOT NULL DEFAULT '',
    square_payment_id text    NOT NULL DEFAULT ''
);
CREATE INDEX idx_driver_tips_batch ON driver_tips (batch_id);

-- +goose Down
DROP INDEX idx_driver_tips_batch;
DROP TABLE driver_tips;
