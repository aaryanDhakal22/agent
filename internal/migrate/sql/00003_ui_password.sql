-- +goose Up
-- +goose StatementBegin
-- Local UI password gate. After the agent is provisioned by an admin-minted
-- token (pasted into the setup screen), the operator sets a password whose
-- bcrypt hash lives here; an optional plaintext hint is surfaced behind a
-- "Hint?" link on the login screen. Empty hash ⇒ "needs password" state.
ALTER TABLE settings ADD COLUMN ui_password_hash text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN ui_password_hint text NOT NULL DEFAULT '';

-- Re-architecture: the agent is now provisioned by a token pasted into the setup
-- screen (minted in main's admin dashboard) and gated by a local password. Any
-- pre-existing key was minted by the retired store-manager login flow — force a
-- clean re-provision so every agent goes through the new token → set-password
-- onboarding rather than silently landing mid-flow.
UPDATE settings SET store_api_key = '' WHERE id = 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE settings DROP COLUMN ui_password_hint;
ALTER TABLE settings DROP COLUMN ui_password_hash;
-- +goose StatementEnd
