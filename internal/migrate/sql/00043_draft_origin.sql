-- +goose Up
-- draft_origin (contract/agent.md §2.10ab "The store wins"): who put a shared
-- draft. '' = a store terminal (or the Pi web page), 'callcenter' = a
-- call-center seat: its body is the call-center app's own format, so a store
-- terminal never lists or opens it (it is the seat's edit lock on an order).
ALTER TABLE terminal_drafts ADD COLUMN origin text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE terminal_drafts DROP COLUMN origin;
