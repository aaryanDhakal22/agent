-- Orders -------------------------------------------------------------------

-- name: UpsertArrivedOrder :execrows
-- Idempotent: SSE redelivery (or a reconnect-backlog replay) for the same
-- canonical id is a no-op so we don't reset state to 'arrived'. The order joins
-- the currently open batch; arrival_date falls back to the column DEFAULT.
INSERT INTO orders (id, tenant_id, location_id, source, source_event_id, snapshot, state, batch_id)
VALUES (?, ?, ?, ?, ?, ?, 'arrived',
        (SELECT id FROM batches WHERE closed_at IS NULL ORDER BY id DESC LIMIT 1))
ON CONFLICT (id) DO NOTHING;

-- name: GetOrderByID :one
SELECT * FROM orders WHERE id = ?;

-- name: ListOrdersPage :many
SELECT * FROM orders
ORDER BY arrival_date DESC
LIMIT ? OFFSET ?;

-- name: ListOrdersByBatch :many
SELECT * FROM orders
WHERE batch_id = ?
ORDER BY arrival_date DESC
LIMIT ? OFFSET ?;

-- name: ListArrivedOrders :many
SELECT * FROM orders
WHERE state = 'arrived'
ORDER BY arrival_date ASC;

-- name: ClaimAccept :execrows
-- Claims an arrived order for acceptance (0 rows = missing or already taken).
-- The kitchen number is kept across a revert + re-accept.
UPDATE orders
SET state              = 'accepted',
    printed_date       = COALESCE(printed_date, sqlc.arg(now)),
    accepted_at        = sqlc.arg(now),
    prep_minutes       = sqlc.arg(prep_minutes),
    estimated_ready_at = sqlc.arg(estimated_ready_at),
    kitchen_number     = COALESCE(kitchen_number, sqlc.arg(kitchen_number)),
    accept_pending     = 1
WHERE id = sqlc.arg(id) AND state = 'arrived';

-- name: RevertAccept :exec
-- Undoes a claim whose tickets printed nowhere, so the order can be re-accepted.
UPDATE orders
SET state              = 'arrived',
    printed_date       = NULL,
    accepted_at        = NULL,
    estimated_ready_at = NULL,
    accept_pending     = 0
WHERE id = ? AND state = 'accepted';

-- name: ClearAcceptPending :exec
UPDATE orders SET accept_pending = 0 WHERE id = ?;

-- name: ListInterruptedAccepts :many
-- Claimed but never recorded: the agent stopped between claim and record.
SELECT id FROM orders WHERE state = 'accepted' AND accept_pending = 1;

-- name: MarkReady :execrows
UPDATE orders SET state = 'ready', ready_at = ?
WHERE id = ? AND state = 'accepted';

-- name: MarkOnDelivery :execrows
-- Leaving with a driver empties the order's pool slot.
UPDATE orders
SET state = 'on_delivery', out_at = ?, driver_id = ?, driver_name = ?,
    pool_driver_id = NULL, pooled_at = NULL, pool_seq = NULL, pool_plan_id = NULL
WHERE id = ? AND state = 'ready'
  AND json_extract(snapshot, '$.fulfillment.type') = 'delivery';

-- name: ListOnDeliveryByDriver :many
SELECT * FROM orders
WHERE state = 'on_delivery' AND driver_id = ?
ORDER BY out_at ASC;

-- name: MarkDelivered :execrows
UPDATE orders SET state = 'delivered', delivered_at = ?
WHERE id = ? AND state = 'on_delivery';

-- name: ListBatchLeftovers :many
-- Orders of a batch that aren't finished: finished = delivered or cancelled,
-- or ready for a non-delivery order.
SELECT * FROM orders
WHERE batch_id = ?
  AND NOT (state IN ('delivered', 'cancelled')
           OR (state = 'ready'
               AND COALESCE(json_extract(snapshot, '$.fulfillment.type'), '') <> 'delivery'))
ORDER BY arrival_date ASC;

-- name: CancelOrder :exec
-- Close-batch: an order nobody accepted is cancelled (never printed, no kitchen number).
UPDATE orders SET state = 'cancelled', cancelled_at = ?, pool_driver_id = NULL, pooled_at = NULL, pool_seq = NULL, pool_plan_id = NULL
WHERE id = ? AND state = 'arrived';

-- name: FinishOrder :exec
-- Close-batch auto-finish: force an order to its terminal state.
UPDATE orders
SET state = ?, accepted_at = ?, ready_at = ?, delivered_at = ?, pool_driver_id = NULL, pooled_at = NULL, pool_seq = NULL, pool_plan_id = NULL
WHERE id = ?;

-- name: ListOverdueArrived :many
-- Arrived orders older than the cutoff that haven't been alerted since the cutoff.
SELECT * FROM orders
WHERE state = 'arrived'
  AND arrival_date <= sqlc.arg(cutoff)
  AND (last_alert_at IS NULL OR last_alert_at <= sqlc.arg(cutoff))
ORDER BY arrival_date ASC;

-- name: MarkAlerted :exec
UPDATE orders SET last_alert_at = ? WHERE id = ?;

-- name: DeleteOrdersOutsideKeptBatches :execrows
-- Batch retention: keep the open batch plus the newest N closed batches.
DELETE FROM orders
WHERE batch_id NOT IN (SELECT id FROM batches WHERE closed_at IS NULL)
  AND batch_id NOT IN (
      SELECT id FROM batches WHERE closed_at IS NOT NULL ORDER BY id DESC LIMIT ?
  );

-- name: DeleteOldClosedBatches :exec
DELETE FROM batches
WHERE closed_at IS NOT NULL
  AND id NOT IN (
      SELECT id FROM batches WHERE closed_at IS NOT NULL ORDER BY id DESC LIMIT ?
  );

-- Batches -------------------------------------------------------------------

-- name: GetOpenBatch :one
SELECT * FROM batches WHERE closed_at IS NULL ORDER BY id DESC LIMIT 1;

-- name: CloseBatch :exec
UPDATE batches SET closed_at = ? WHERE id = ?;

-- name: OpenBatch :one
INSERT INTO batches (opened_at) VALUES (?) RETURNING *;

-- name: ListBatches :many
SELECT b.id, b.opened_at, b.closed_at,
       (SELECT count(*) FROM orders o WHERE o.batch_id = b.id) AS order_count
FROM batches b
ORDER BY b.id DESC
LIMIT ?;

-- Drivers -------------------------------------------------------------------

-- name: ListActiveDrivers :many
SELECT * FROM drivers WHERE active ORDER BY position ASC, created_at ASC;

-- name: GetDriver :one
SELECT * FROM drivers WHERE id = ?;

-- name: NextDriverPosition :one
SELECT CAST(COALESCE(MAX(position), 0) + 1 AS INTEGER) AS next_position FROM drivers;

-- name: CreateDriver :exec
INSERT INTO drivers (id, name, position) VALUES (?, ?, ?);

-- name: RenameDriver :execrows
UPDATE drivers SET name = ? WHERE id = ? AND active;

-- name: DeactivateDriver :execrows
UPDATE drivers SET active = false WHERE id = ? AND active;

-- name: SetDriverPosition :exec
UPDATE drivers SET position = ? WHERE id = ?;

-- name: SetDriverOut :exec
UPDATE drivers SET status = 'out' WHERE id = ?;

-- name: SetDriverReturned :exec
-- Back from a run: available again, at the back of the queue.
UPDATE drivers
SET status = 'available', last_returned_at = ?, position = ?
WHERE id = ?;

-- name: ResetDriversAvailable :exec
-- Close batch: drivers out on a run are back in the queue (clocked-out stay off).
UPDATE drivers SET status = 'available' WHERE status = 'out';

-- name: SetDriverStatus :execrows
UPDATE drivers SET status = sqlc.arg(new_status), position = sqlc.arg(position)
WHERE id = sqlc.arg(id) AND active AND status = sqlc.arg(old_status);

-- name: ClockOutDriver :execrows
UPDATE drivers SET status = 'off' WHERE id = ? AND status = 'available';

-- name: ListAvailableDrivers :many
SELECT * FROM drivers WHERE active AND status = 'available';

-- Driver shifts ----------------------------------------------------------------

-- name: OpenShift :exec
INSERT INTO driver_shifts (id, driver_id, driver_name, batch_id, clock_in) VALUES (?, ?, ?, ?, ?);

-- name: GetOpenShift :one
SELECT * FROM driver_shifts WHERE driver_id = ? AND clock_out IS NULL ORDER BY clock_in DESC LIMIT 1;

-- name: CloseShift :exec
UPDATE driver_shifts SET clock_out = ?, auto_out = ? WHERE id = ?;

-- name: ListOpenShifts :many
SELECT * FROM driver_shifts WHERE clock_out IS NULL;

-- name: ListShiftsSince :many
SELECT * FROM driver_shifts WHERE clock_in >= ? ORDER BY clock_in DESC;

-- name: GetDriverReset :one
SELECT driver_reset_time, last_driver_reset_at FROM settings WHERE id = 1;

-- name: SetDriverResetTime :exec
UPDATE settings SET driver_reset_time = ? WHERE id = 1;

-- name: SetLastDriverReset :exec
UPDATE settings SET last_driver_reset_at = ? WHERE id = 1;

-- name: GetFloorPrefs :one
SELECT green_till_minutes, tip_tax_bps FROM settings WHERE id = 1;

-- name: SetFloorPrefs :exec
UPDATE settings SET green_till_minutes = ?, tip_tax_bps = ? WHERE id = 1;

-- name: GetReceiptPrefs :one
SELECT receipt_prefs FROM settings WHERE id = 1;

-- name: SetReceiptPrefs :exec
UPDATE settings SET receipt_prefs = ? WHERE id = 1;

-- name: SetLastReturn :exec
UPDATE settings SET last_return_driver_id = ?, last_return_position = ?, last_return_at = ? WHERE id = 1;

-- name: GetLastReturn :one
SELECT last_return_driver_id, last_return_position, last_return_at FROM settings WHERE id = 1;

-- name: SetDriverOutAt :execrows
-- Undo a mistaken return: back out on the run, at its old queue position.
UPDATE drivers SET status = 'out', position = ? WHERE id = ? AND active AND status = 'available';

-- name: GetAppRelease :one
SELECT app_release FROM settings WHERE id = 1;

-- name: SetAppRelease :exec
UPDATE settings SET app_release = ? WHERE id = 1;

-- name: GetBatch :one
SELECT * FROM batches WHERE id = ?;

-- name: ListBatchDeliveries :many
-- Driver report: delivered orders of a batch that went out with a driver.
SELECT * FROM orders
WHERE batch_id = ? AND state = 'delivered' AND driver_id IS NOT NULL
ORDER BY delivered_at ASC, kitchen_number ASC;

-- name: ListShiftsOverlapping :many
-- Driver report: shifts that overlap a batch window (opened_at .. closed_at).
SELECT * FROM driver_shifts
WHERE clock_in <= sqlc.arg(window_end) AND (clock_out IS NULL OR clock_out >= sqlc.arg(window_start))
ORDER BY clock_in ASC;

-- name: CountDeliveriesInBatch :many
SELECT driver_id, count(*) AS deliveries FROM orders
WHERE batch_id = ? AND driver_id IS NOT NULL
GROUP BY driver_id;

-- Floor events (sync outbox to main) -------------------------------------------

-- name: InsertFloorEvent :exec
INSERT INTO floor_events (id, order_id, type, payload, at) VALUES (?, ?, ?, ?, ?);

-- name: ListUnsyncedFloorEvents :many
SELECT * FROM floor_events WHERE synced_at IS NULL ORDER BY rowid ASC LIMIT ?;

-- name: MarkFloorEventSynced :exec
UPDATE floor_events SET synced_at = ?, last_error = '' WHERE id = ?;

-- name: MarkFloorEventFailed :exec
UPDATE floor_events SET attempts = attempts + 1, last_error = ? WHERE id = ?;

-- name: CountUnsyncedFloorEvents :one
SELECT count(*) FROM floor_events WHERE synced_at IS NULL;

-- name: DeleteOldSyncedFloorEvents :execrows
DELETE FROM floor_events WHERE synced_at IS NOT NULL AND synced_at < ?;

-- Paired devices (counter tablets) -------------------------------------------

-- name: CreatePairedDevice :exec
INSERT INTO paired_devices (id, name, key_hash, key_prefix) VALUES (?, ?, ?, ?);

-- name: GetPairedDeviceByHash :one
SELECT * FROM paired_devices WHERE key_hash = ? AND revoked_at IS NULL;

-- name: TouchPairedDevice :exec
UPDATE paired_devices SET last_seen_at = ? WHERE id = ?;

-- name: ListPairedDevices :many
SELECT * FROM paired_devices ORDER BY created_at DESC;

-- name: RevokeAllPairedDevices :exec
UPDATE paired_devices SET revoked_at = ? WHERE revoked_at IS NULL;

-- name: RevokePairedDevice :execrows
UPDATE paired_devices SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL;

-- name: RenamePairedDevice :execrows
UPDATE paired_devices SET name = ? WHERE id = ? AND revoked_at IS NULL;

-- Settings -----------------------------------------------------------------

-- name: GetAutoAccept :one
SELECT auto_accept FROM settings WHERE id = 1;

-- name: SetAutoAccept :exec
INSERT INTO settings (id, auto_accept, updated_at)
VALUES (1, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
ON CONFLICT (id) DO UPDATE SET
    auto_accept = excluded.auto_accept,
    updated_at  = excluded.updated_at;

-- name: GetStoreAPIKey :one
SELECT store_api_key FROM settings WHERE id = 1;

-- name: SetStoreAPIKey :exec
INSERT INTO settings (id, store_api_key, updated_at)
VALUES (1, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
ON CONFLICT (id) DO UPDATE SET
    store_api_key = excluded.store_api_key,
    updated_at    = excluded.updated_at;

-- name: GetSessionSecret :one
SELECT session_secret FROM settings WHERE id = 1;

-- name: SetSessionSecret :exec
UPDATE settings
SET session_secret = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = 1;

-- name: GetUIPassword :one
SELECT ui_password_hash, ui_password_hint FROM settings WHERE id = 1;

-- name: SetUIPassword :exec
UPDATE settings
SET ui_password_hash = ?, ui_password_hint = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = 1;

-- name: GetFloorSettings :one
SELECT kitchen_next, last_prep_minutes FROM settings WHERE id = 1;

-- name: NextKitchenNumber :one
-- Allocates a kitchen number: returns the post-increment value (caller subtracts 1).
UPDATE settings SET kitchen_next = kitchen_next + 1 WHERE id = 1 RETURNING kitchen_next;

-- name: ResetKitchenNumber :exec
UPDATE settings SET kitchen_next = 1100 WHERE id = 1;

-- name: SetLastPrepMinutes :exec
UPDATE settings SET last_prep_minutes = ? WHERE id = 1;

-- name: ClearProvisioning :exec
-- Unlink / definitive revoke: drop the store key AND the local password + hint
-- in one statement, back to the blank "paste a token" state.
UPDATE settings
SET store_api_key = '', ui_password_hash = '', ui_password_hint = '',
    secret_pushover_app_token = '', secret_pushover_user_key = '', secret_pushover_boss_key = '',
    secret_otlp_endpoint = '', secret_otlp_headers = '', manager_username = '', device_id = '',
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = 1;

-- Store config (starter pack from main/'s SSE handshake) --------------------

-- name: GetStoreConfig :one
SELECT context, updated_at FROM store_config WHERE id = 1;

-- name: UpsertStoreConfig :exec
INSERT INTO store_config (id, context, updated_at)
VALUES (1, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
ON CONFLICT (id) DO UPDATE SET
    context    = excluded.context,
    updated_at = excluded.updated_at;

-- Link + restore (00008) --------------------------------------------------------

-- name: SetLinkInfo :exec
UPDATE settings
SET secret_pushover_app_token = ?, secret_pushover_user_key = ?, secret_pushover_boss_key = ?,
    secret_otlp_endpoint = ?, secret_otlp_headers = ?,
    manager_username = ?, device_id = ?, ui_password_hash = ?, ui_password_hint = ?
WHERE id = 1;

-- name: GetLinkInfo :one
SELECT secret_pushover_app_token, secret_pushover_user_key, secret_pushover_boss_key,
       secret_otlp_endpoint, secret_otlp_headers, manager_username, device_id
FROM settings WHERE id = 1;

-- name: SetManagerPasswordHash :exec
UPDATE settings SET ui_password_hash = ? WHERE id = 1;

-- name: GetLastBackup :one
SELECT last_backup_at FROM settings WHERE id = 1;

-- name: SetLastBackup :exec
UPDATE settings SET last_backup_at = ? WHERE id = 1;

-- name: CountAllOrders :one
SELECT count(*) FROM orders;

-- name: CountAllDrivers :one
SELECT count(*) FROM drivers;

-- name: DeleteAllBatches :exec
DELETE FROM batches;

-- name: InsertBatchWithID :exec
INSERT INTO batches (id, opened_at) VALUES (?, ?);

-- name: RestoreOrder :exec
INSERT INTO orders (id, tenant_id, location_id, source, source_event_id, snapshot, state, arrival_date,
                    printed_date, batch_id, kitchen_number, prep_minutes, estimated_ready_at, accepted_at,
                    ready_at, out_at, delivered_at, driver_id, driver_name, cancelled_at,
                    pool_driver_id, pooled_at, pool_seq, pool_plan_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: RestoreDriver :exec
INSERT INTO drivers (id, name, active, position, status, last_returned_at) VALUES (?, ?, true, ?, ?, ?);

-- name: RestoreShift :exec
INSERT INTO driver_shifts (id, driver_id, driver_name, batch_id, clock_in) VALUES (?, ?, ?, ?, ?);

-- name: SetKitchenCounters :exec
UPDATE settings SET kitchen_next = ?, last_prep_minutes = ? WHERE id = 1;

-- Pi updates (2.0.6) ----------------------------------------------------------

-- name: CountOpenBatchOrders :one
SELECT CAST(count(*) AS INTEGER) AS total,
       CAST(COALESCE(SUM(CASE WHEN state IN ('arrived', 'accepted', 'ready', 'on_delivery') THEN 1 ELSE 0 END), 0) AS INTEGER) AS active
FROM orders
WHERE batch_id = (SELECT id FROM batches WHERE closed_at IS NULL ORDER BY id DESC LIMIT 1);

-- name: GetUpdateState :one
SELECT update_job, update_notified FROM settings WHERE id = 1;

-- name: SetUpdateJob :exec
UPDATE settings SET update_job = ? WHERE id = 1;

-- name: SetUpdateNotified :exec
UPDATE settings SET update_notified = ? WHERE id = 1;

-- Printer MACs (2.0.6) --------------------------------------------------------

-- name: ListPrinterMACs :many
SELECT printer_id, mac, ip, seen_at FROM printer_macs ORDER BY printer_id;

-- name: UpsertPrinterMAC :exec
INSERT INTO printer_macs (printer_id, mac, ip, seen_at) VALUES (?, ?, ?, ?)
ON CONFLICT (printer_id) DO UPDATE SET mac = excluded.mac, ip = excluded.ip, seen_at = excluded.seen_at;

-- Driver pools (2.0.9) ----------------------------------------------------------

-- name: SetOrderPool :execrows
-- Puts a delivery order (accepted or ready) into a driver's pool, or back to
-- unassigned with a NULL driver. 0 rows = not a pool-able order.
-- The order's stop number is cleared; the service renumbers the pools.
UPDATE orders SET pool_driver_id = sqlc.narg(pool_driver_id), pooled_at = sqlc.narg(pooled_at), pool_seq = NULL,
    pool_plan_id = sqlc.narg(pool_plan_id)
WHERE id = sqlc.arg(id) AND state IN ('accepted', 'ready')
  AND json_extract(snapshot, '$.fulfillment.type') = 'delivery';

-- name: ListPoolByDriver :many
SELECT * FROM orders WHERE pool_driver_id = ?
ORDER BY pool_seq IS NULL, pool_seq ASC, pooled_at ASC, arrival_date ASC;

-- name: ClearOrderPool :exec
UPDATE orders SET pool_driver_id = NULL, pooled_at = NULL, pool_seq = NULL, pool_plan_id = NULL WHERE id = ?;

-- name: SetPoolSeq :execrows
-- Renumbers one pooled order (0 rows = no longer in that pool, or unchanged).
UPDATE orders SET pool_seq = sqlc.arg(pool_seq)
WHERE id = sqlc.arg(id) AND pool_driver_id = sqlc.arg(pool_driver_id)
  AND (pool_seq IS NULL OR pool_seq <> sqlc.arg(pool_seq));

-- name: ListOpenDeliveries :many
-- The open batch's deliveries still on the floor (cooking, ready or out).
SELECT * FROM orders
WHERE batch_id = (SELECT id FROM batches WHERE closed_at IS NULL ORDER BY id DESC LIMIT 1)
  AND state IN ('accepted', 'ready', 'on_delivery')
  AND json_extract(snapshot, '$.fulfillment.type') = 'delivery'
ORDER BY arrival_date ASC;

-- Order geo (main's order.geo) ----------------------------------------------------

-- name: UpsertOrderGeo :exec
INSERT INTO order_geo (order_id, state, lat, lng, quality, distance_m, duration_s, polyline,
                       outside_area, store_to_s, to_store_s, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (order_id) DO UPDATE SET
    state = excluded.state, lat = excluded.lat, lng = excluded.lng, quality = excluded.quality,
    distance_m = excluded.distance_m, duration_s = excluded.duration_s, polyline = excluded.polyline,
    outside_area = excluded.outside_area, store_to_s = excluded.store_to_s,
    to_store_s = excluded.to_store_s, updated_at = excluded.updated_at;

-- name: GetOrderGeo :one
SELECT * FROM order_geo WHERE order_id = ?;

-- name: ListOrderGeo :many
SELECT * FROM order_geo WHERE order_id IN (sqlc.slice('ids'));

-- name: UpsertOrderTime :exec
INSERT INTO order_times (a, b, seconds) VALUES (?, ?, ?)
ON CONFLICT (a, b) DO UPDATE SET seconds = excluded.seconds;

-- name: ListOrderTimes :many
-- Drive times between the given orders (both ends in the set).
SELECT a, b, seconds FROM order_times
WHERE a IN (sqlc.slice('from_ids')) AND b IN (sqlc.slice('to_ids'));

-- name: DeleteOrphanOrderGeo :exec
DELETE FROM order_geo WHERE order_id NOT IN (SELECT id FROM orders);

-- name: DeleteOrphanOrderTimes :exec
DELETE FROM order_times
WHERE a NOT IN (SELECT id FROM orders) OR b NOT IN (SELECT id FROM orders);

-- AUTO plans ------------------------------------------------------------------------

-- name: InsertAutoPlan :exec
INSERT INTO auto_plans (id, created_at, plan_json) VALUES (?, ?, ?);

-- name: GetAutoPlan :one
SELECT * FROM auto_plans WHERE id = ?;

-- name: ConfirmAutoPlan :exec
UPDATE auto_plans SET confirmed_at = COALESCE(confirmed_at, ?) WHERE id = ?;

-- name: DeleteOldAutoPlans :exec
DELETE FROM auto_plans WHERE created_at < ?;

-- Delivery settings + store map (2.0.9) -------------------------------------------

-- name: GetDeliveryPrefs :one
SELECT soon_ready_minutes, delivery_target_minutes FROM settings WHERE id = 1;

-- name: SetDeliveryPrefs :exec
UPDATE settings SET soon_ready_minutes = ?, delivery_target_minutes = ? WHERE id = 1;

-- name: GetMapFile :one
SELECT map_file FROM settings WHERE id = 1;

-- name: SetMapFile :exec
UPDATE settings SET map_file = ? WHERE id = 1;
