-- +goose Up
-- print_routing: the store's printer routing + emergency reroute setting
-- (JSON {mode, emergency_printer_id, targets}; '' = mode none, the automatic
-- failover only). contract/agent.md §2.3c, spec §4.42.
ALTER TABLE settings ADD COLUMN print_routing text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE settings DROP COLUMN print_routing;
