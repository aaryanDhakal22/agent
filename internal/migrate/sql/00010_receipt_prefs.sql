-- +goose Up
-- Receipt settings the store manager changes from the tablet (kitchen ticket
-- text sizes, receipt footer, copies, a few on/off lines). Stored as one JSON
-- object; '{}' (and any key missing from an older object) means the default,
-- which prints exactly like the agent did before these settings existed.
ALTER TABLE settings ADD COLUMN receipt_prefs text NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE settings DROP COLUMN receipt_prefs;
