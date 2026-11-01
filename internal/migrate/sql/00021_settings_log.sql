-- +goose Up
-- Settings history (2.3.11): one row per settings PUT that changed
-- something - which section, who (a paired tablet by name / the Pi web
-- page), and each changed key old -> new (JSON). Kept 90 days.
CREATE TABLE settings_log (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  at         TEXT NOT NULL,
  section    TEXT NOT NULL,
  by_kind    TEXT NOT NULL,
  by_name    TEXT NOT NULL DEFAULT '',
  device_id  TEXT NOT NULL DEFAULT '',
  changes    TEXT NOT NULL
);
CREATE INDEX settings_log_at ON settings_log(at);

-- +goose Down
DROP INDEX settings_log_at;
DROP TABLE settings_log;
