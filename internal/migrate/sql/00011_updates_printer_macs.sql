-- +goose Up
-- 2.0.6. printer_macs: the hardware address the Pi learned for each roster
-- printer (from the kernel ARP table after a successful probe), so a printer
-- that moved to a new DHCP address can be found again by scanning the LAN.
-- Written only when the MAC or the IP changes (SD-card writes).
CREATE TABLE printer_macs (
    printer_id text PRIMARY KEY,
    mac        text NOT NULL,
    ip         text NOT NULL,
    seen_at    text NOT NULL
);

-- update_job: the latest Pi update/rollback request (JSON) - a forced update
-- waiting for the store to go idle survives a restart, and the restarted
-- agent still knows who asked and whether it was forced. update_notified: the
-- "<id>:<state>" whose Pushover was already sent (one per terminal state, even
-- across the restart an update causes).
ALTER TABLE settings ADD COLUMN update_job text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN update_notified text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE settings DROP COLUMN update_notified;
ALTER TABLE settings DROP COLUMN update_job;
DROP TABLE printer_macs;
