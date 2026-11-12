-- +goose Up
-- payment_cards (contract/agent.md 2.10r): what Square answered for a card the
-- terminal's reader or manual entry took (auth code, entry, EMV labels,
-- Square's payment id + receipt number), or for a refund given through Square
-- (Square's refund id + the payment it was on), as JSON. One row per payment
-- that has any; never a card number. A table of its own so order_payments'
-- SELECT * stays as it is.
CREATE TABLE payment_cards (
    payment_id text PRIMARY KEY,
    card       text NOT NULL
);

-- +goose Down
DROP TABLE payment_cards;
