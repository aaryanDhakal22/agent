-- +goose Up
-- Shared drafts for the POS terminals (contract/agent.md §2.10i): an order
-- put aside (held) or open on one terminal, so both terminals see it (hold +
-- TAKE OVER). The draft itself is the terminal's own JSON (opaque here).
-- holder_*: the terminal that has it open ('' = held, nobody has it open).
-- location_id: the store it belongs to. Cleared at Close batch; never sent
-- to main.
CREATE TABLE terminal_drafts (
    id                 text    PRIMARY KEY,
    location_id        text    NOT NULL DEFAULT '',
    type               text    NOT NULL,
    name               text    NOT NULL DEFAULT '',
    held               boolean NOT NULL DEFAULT true,
    holder_device_id   text    NOT NULL DEFAULT '',
    holder_device_name text    NOT NULL DEFAULT '',
    holder_staff_id    text    NOT NULL DEFAULT '',
    holder_staff_name  text    NOT NULL DEFAULT '',
    staff_id           text    NOT NULL DEFAULT '',
    staff_name         text    NOT NULL DEFAULT '',
    order_id           text    NOT NULL DEFAULT '',
    draft              text    NOT NULL DEFAULT '{}',
    created_at         text    NOT NULL,
    updated_at         text    NOT NULL
);

-- +goose Down
DROP TABLE terminal_drafts;
