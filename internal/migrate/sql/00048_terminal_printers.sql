-- +goose Up
-- terminal_printers (contract/agent.md 2.10ap, the owner 2026-11-14: "no tickets
-- on the printer attached to the POS"): the Pi keeps a copy of each counter
-- terminal's receipt printer settings (the printer test's width, characters,
-- styles, logo / crossed-out answers, the confirmed drawer command, the
-- reroute / backup choice), so a terminal that lost its own (a reinstall, a
-- factory reset) gets them back instead of printing at 48 columns with no
-- drawer. One row per printer: printer_key = the terminal's profile key
-- (vid:pid[:serial] for USB, 0000:0000:<host>:<port> for a network printer).
-- profile / route are the terminal's JSON, kept as sent (<= 8 KB each).
CREATE TABLE terminal_printers (
    printer_key text PRIMARY KEY,
    device_id   text NOT NULL,
    device_name text NOT NULL DEFAULT '',
    profile     text NOT NULL,
    route       text NOT NULL DEFAULT '',
    updated_at  text NOT NULL
);

-- +goose Down
DROP TABLE terminal_printers;
