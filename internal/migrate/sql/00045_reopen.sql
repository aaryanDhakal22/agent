-- +goose Up
-- Reopen a check from the terminal's All checks (contract/agent.md §2.10ak,
-- batch B, the owner 2026-11-14): when a cashier last reopened it (null =
-- never). A check is open again while nothing settled it since (paid_at,
-- finished_at, out_at, delivered_at after it): a pickup / counter check is
-- back on the Pickup list, a delivery under Dispatch's New.
ALTER TABLE orders ADD COLUMN reopened_at text;

-- +goose Down
ALTER TABLE orders DROP COLUMN reopened_at;
