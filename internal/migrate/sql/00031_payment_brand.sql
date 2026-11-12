-- +goose Up
-- The till report (contract/agent.md §2.10m): a card payment's brand
-- (amex | discover | mastercard | visa | other) as the terminal sends it;
-- '' = none given (counted as Credit Other) or not a card.
ALTER TABLE order_payments ADD COLUMN brand text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE order_payments DROP COLUMN brand;
