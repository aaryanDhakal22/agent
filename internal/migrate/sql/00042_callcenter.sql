-- +goose Up
-- callcenter (contract/agent.md §2.10ab): paired_devices.kind gains
-- 'callcenter', the store's one virtual call-center device (id
-- 'callcenter', no key: main relays the call-center app's calls to it).
-- SQLite can't change a CHECK: rebuilt as 00025 / 00039 did (nothing
-- references it).
CREATE TABLE paired_devices_new (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    key_hash     text NOT NULL UNIQUE,
    key_prefix   text NOT NULL,
    created_at   text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at text,
    revoked_at   text,
    kind         text NOT NULL DEFAULT 'tablet' CHECK (kind IN ('tablet', 'integration', 'terminal', 'phone', 'callcenter')),
    app_version         text NOT NULL DEFAULT '',
    app_version_seen_at text
);
INSERT INTO paired_devices_new (id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at)
SELECT id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at FROM paired_devices;
DROP TABLE paired_devices;
ALTER TABLE paired_devices_new RENAME TO paired_devices;

-- +goose Down
DELETE FROM paired_devices WHERE kind = 'callcenter';
CREATE TABLE paired_devices_old (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    key_hash     text NOT NULL UNIQUE,
    key_prefix   text NOT NULL,
    created_at   text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at text,
    revoked_at   text,
    kind         text NOT NULL DEFAULT 'tablet' CHECK (kind IN ('tablet', 'integration', 'terminal', 'phone')),
    app_version         text NOT NULL DEFAULT '',
    app_version_seen_at text
);
INSERT INTO paired_devices_old (id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at)
SELECT id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at FROM paired_devices;
DROP TABLE paired_devices;
ALTER TABLE paired_devices_old RENAME TO paired_devices;
