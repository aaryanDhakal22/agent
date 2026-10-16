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
    kitchen_number     = COALESCE(kitchen_number, sqlc.arg(kitchen_number))
WHERE id = sqlc.arg(id) AND state = 'arrived';

-- name: RevertAccept :exec
-- Undoes a claim whose tickets printed nowhere, so the order can be re-accepted.
UPDATE orders
SET state              = 'arrived',
    printed_date       = NULL,
    accepted_at        = NULL,
    estimated_ready_at = NULL
WHERE id = ? AND state = 'accepted';

-- name: MarkReady :execrows
UPDATE orders SET state = 'ready', ready_at = ?
WHERE id = ? AND state = 'accepted';

-- name: MarkOnDelivery :execrows
UPDATE orders
SET state = 'on_delivery', out_at = ?, driver_id = ?, driver_name = ?
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
UPDATE orders SET state = 'cancelled', cancelled_at = ? WHERE id = ? AND state = 'arrived';

-- name: FinishOrder :exec
-- Close-batch auto-finish: force an order to its terminal state.
UPDATE orders
SET state = ?, accepted_at = ?, ready_at = ?, delivered_at = ?
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
