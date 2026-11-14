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
-- The kitchen number is kept across a revert + re-accept; a future order's
-- placeholder is replaced by the number taken (contract 2.10ae).
UPDATE orders
SET state              = 'accepted',
    printed_date       = COALESCE(printed_date, sqlc.arg(now)),
    accepted_at        = sqlc.arg(now),
    prep_minutes       = sqlc.arg(prep_minutes),
    estimated_ready_at = sqlc.arg(estimated_ready_at),
    kitchen_number     = CASE WHEN number_placeholder = 1 OR kitchen_number IS NULL THEN sqlc.arg(kitchen_number) ELSE kitchen_number END,
    number_placeholder = 0,
    accept_pending     = 1,
    auto_accepted      = sqlc.arg(auto_accepted)
WHERE id = sqlc.arg(id) AND state = 'arrived';

-- name: RevertAccept :exec
-- Undoes a claim whose tickets printed nowhere, so the order can be re-accepted.
UPDATE orders
SET state              = 'arrived',
    printed_date       = NULL,
    accepted_at        = NULL,
    estimated_ready_at = NULL,
    accept_pending     = 0,
    auto_accepted      = false
WHERE id = ? AND state = 'accepted';

-- name: ClearAcceptPending :exec
UPDATE orders SET accept_pending = 0 WHERE id = ?;

-- name: ListInterruptedAccepts :many
-- Claimed but never recorded: the agent stopped between claim and record.
SELECT id FROM orders WHERE state = 'accepted' AND accept_pending = 1;

-- name: MarkReady :execrows
UPDATE orders SET state = 'ready', ready_at = ?
WHERE id = ? AND state = 'accepted';

-- name: UndoReady :execrows
-- 2.3.8: a READY tapped by mistake, undone soon after (ready_at >= since):
-- back to cooking, not finished. Pools stay (a cooking order may be pooled).
UPDATE orders SET state = 'accepted', ready_at = NULL, finished_at = NULL, finished_by = ''
WHERE id = sqlc.arg(id) AND state = 'ready' AND NOT came_back AND ready_at >= sqlc.arg(since);

-- name: MarkOnDelivery :execrows
-- Leaving with a driver empties the order's pool slot (and ends a came-back
-- hold: only the POS mirror sends a came-back order out).
UPDATE orders
SET state = 'on_delivery', out_at = ?, driver_id = ?, driver_name = ?,
    pool_driver_id = NULL, pooled_at = NULL, pool_seq = NULL, pool_plan_id = NULL, came_back = false
WHERE id = ? AND state = 'ready'
  AND json_extract(snapshot, '$.fulfillment.type') = 'delivery';

-- name: RecallOrder :execrows
-- 2.4.1: an undelivered order of a recalled run goes back to its driver's
-- pool, ready, keeping its stop number (run_seq is rewritten on the next run).
UPDATE orders
SET state = 'ready', out_at = NULL, driver_id = NULL, driver_name = '',
    pool_driver_id = sqlc.arg(pool_driver_id), pooled_at = sqlc.arg(pooled_at),
    pool_seq = sqlc.arg(pool_seq), pool_plan_id = NULL
WHERE id = sqlc.arg(id) AND state = 'on_delivery';

-- name: FrontDriverPosition :one
-- 2.4.1: a recalled driver is next in the queue.
SELECT CAST(COALESCE(MIN(position), 1) - 1 AS INTEGER) AS front_position FROM drivers;

-- name: ListOnDeliveryByDriver :many
SELECT * FROM orders
WHERE state = 'on_delivery' AND driver_id = ?
ORDER BY out_at ASC;

-- name: MarkDelivered :execrows
UPDATE orders SET state = 'delivered', delivered_at = ?
WHERE id = ? AND state = 'on_delivery';

-- name: MarkNotDelivered :execrows
-- 2.2.1: a delivery that came back with its driver leaves the run. retry =
-- ready again with no driver, held aside (came_back) until released; cancel =
-- cancelled. Either way no driver, no pool, no out / delivered time, so it
-- counts on no driver report.
UPDATE orders
SET state = sqlc.arg(state), came_back = sqlc.arg(came_back), cancelled_at = sqlc.narg(cancelled_at),
    driver_id = NULL, driver_name = '', out_at = NULL, delivered_at = NULL, run_seq = NULL,
    pool_driver_id = NULL, pooled_at = NULL, pool_seq = NULL, pool_plan_id = NULL,
    not_delivered_action = sqlc.arg(action), not_delivered_at = sqlc.arg(at),
    not_delivered_driver_id = sqlc.arg(from_driver_id)
WHERE id = sqlc.arg(id) AND state IN ('on_delivery', 'delivered')
  AND json_extract(snapshot, '$.fulfillment.type') = 'delivery';

-- name: ReleaseCameBack :execrows
-- A came-back order is released: a normal unassigned ready delivery again.
UPDATE orders SET came_back = false WHERE id = ? AND came_back AND state = 'ready';

-- name: CancelCameBack :execrows
-- Close batch: a came-back order nobody took out again never reached the
-- customer - cancelled (not auto-delivered).
UPDATE orders
SET state = 'cancelled', cancelled_at = sqlc.arg(cancelled_at), came_back = false,
    not_delivered_action = 'cancel', not_delivered_at = sqlc.arg(cancelled_at),
    pool_driver_id = NULL, pooled_at = NULL, pool_seq = NULL, pool_plan_id = NULL
WHERE id = sqlc.arg(id) AND came_back AND state = 'ready';

-- name: SetRunSeq :exec
UPDATE orders SET run_seq = ? WHERE id = ?;

-- name: NextRunSeq :one
-- A stop joining a run that is out goes last.
SELECT CAST(COALESCE(MAX(run_seq), 0) + 1 AS INTEGER) FROM orders WHERE state = 'on_delivery' AND driver_id = ?;

-- name: ListRunStops :many
-- Every driver's run that is out, stops in run order.
SELECT id, driver_id FROM orders
WHERE state = 'on_delivery' AND driver_id IS NOT NULL
ORDER BY driver_id, run_seq IS NULL, run_seq ASC, out_at ASC, arrival_date ASC;

-- name: MarkDeliveredNoDriver :execrows
-- 2.4.0 MARK DELIVERED: an open delivery (cooking or ready, pooled or not,
-- held as came back) is delivered with no driver - Unassigned on the Drivers
-- tab. marked = the MarkedDelivered JSON.
UPDATE orders
SET state = 'delivered', delivered_at = sqlc.arg(at), marked_delivered = sqlc.arg(marked),
    driver_id = NULL, driver_name = '', run_seq = NULL, came_back = false,
    pool_driver_id = NULL, pooled_at = NULL, pool_seq = NULL, pool_plan_id = NULL
WHERE id = sqlc.arg(id) AND state IN ('accepted', 'ready')
  AND json_extract(snapshot, '$.fulfillment.type') = 'delivery';

-- name: UndeliverOrder :execrows
-- 2.4.0 Back to deliveries: an Unassigned delivery goes back to the state it
-- was in (cooking or ready), no longer delivered.
UPDATE orders
SET state = sqlc.arg(state), delivered_at = NULL, marked_delivered = ''
WHERE id = sqlc.arg(id) AND state = 'delivered' AND driver_id IS NULL AND marked_delivered <> '';

-- name: SetOrderDriver :execrows
-- 2.4.0 Drivers tab: a delivered or out delivery moves to another driver
-- (driver_id NULL = Unassigned) with its run stop number and the
-- MarkedDelivered record.
UPDATE orders
SET driver_id = sqlc.narg(driver_id), driver_name = sqlc.arg(driver_name), run_seq = sqlc.narg(run_seq),
    marked_delivered = sqlc.arg(marked_delivered)
WHERE id = sqlc.arg(id) AND state IN ('delivered', 'on_delivery');

-- name: SetEarlyPrinted :exec
-- 2.4.0: a future order's early tickets printed (kitchen / receipt).
UPDATE orders
SET early_kitchen = early_kitchen OR sqlc.arg(kitchen), early_receipt = early_receipt OR sqlc.arg(receipt)
WHERE id = sqlc.arg(id);

-- name: ListBatchUnassigned :many
-- 2.4.0: a batch's deliveries delivered with no driver (the UNASSIGNED report).
SELECT * FROM orders
WHERE batch_id = ? AND state = 'delivered' AND driver_id IS NULL AND marked_delivered <> ''
ORDER BY delivered_at ASC;

-- name: CountOnDeliveryByDriver :one
SELECT CAST(count(*) AS INTEGER) FROM orders WHERE state = 'on_delivery' AND driver_id = ?;

-- name: ListBatchLeftovers :many
-- Orders of a batch that aren't finished: finished = delivered or cancelled,
-- or ready for a non-delivery order. Scheduled (future) orders move on to
-- the next batch instead.
SELECT * FROM orders
WHERE batch_id = ?
  AND state <> 'scheduled'
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
SET state = ?, accepted_at = ?, ready_at = ?, delivered_at = ?, pool_driver_id = NULL, pooled_at = NULL, pool_seq = NULL, pool_plan_id = NULL,
    came_back = false
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
  )
  AND batch_id NOT IN (
      -- F-10: held while an event about them hasn't reached main, or a carried check is open.
      SELECT o2.batch_id FROM orders o2 JOIN floor_events f ON f.subject = o2.id WHERE f.synced_at IS NULL
      UNION SELECT o3.batch_id FROM orders o3 WHERE o3.carried
      UNION SELECT d.batch_id FROM drawer_sessions d JOIN floor_events f2 ON f2.subject = 'drawer:' || d.id WHERE f2.synced_at IS NULL
      UNION SELECT z.batch_id FROM z_reports z JOIN floor_events f3 ON f3.subject = 'z:' || z.batch_id WHERE f3.synced_at IS NULL
  );

-- name: DeleteOldClosedBatches :exec
DELETE FROM batches
WHERE closed_at IS NOT NULL
  AND id NOT IN (
      SELECT id FROM batches WHERE closed_at IS NOT NULL ORDER BY id DESC LIMIT ?
  )
  AND id NOT IN (
      -- F-10: held while an event about them hasn't reached main, or a carried check is open.
      SELECT o2.batch_id FROM orders o2 JOIN floor_events f ON f.subject = o2.id WHERE f.synced_at IS NULL
      UNION SELECT o3.batch_id FROM orders o3 WHERE o3.carried
      UNION SELECT d.batch_id FROM drawer_sessions d JOIN floor_events f2 ON f2.subject = 'drawer:' || d.id WHERE f2.synced_at IS NULL
      UNION SELECT z.batch_id FROM z_reports z JOIN floor_events f3 ON f3.subject = 'z:' || z.batch_id WHERE f3.synced_at IS NULL
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
INSERT INTO drivers (id, name, position, pushover_device) VALUES (?, ?, ?, ?);

-- name: SetDriverPushoverDevice :execrows
UPDATE drivers SET pushover_device = ? WHERE id = ? AND active;

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

-- name: GetPrintRouting :one
SELECT print_routing FROM settings WHERE id = 1;

-- name: SetPrintRouting :exec
UPDATE settings SET print_routing = ? WHERE id = 1;

-- name: GetAdminPair :one
SELECT admin_pair FROM settings WHERE id = 1;

-- name: SetAdminPair :exec
UPDATE settings SET admin_pair = ? WHERE id = 1;

-- name: GetDeviceSetups :one
SELECT device_setups FROM settings WHERE id = 1;

-- name: SetDeviceSetups :exec
UPDATE settings SET device_setups = ? WHERE id = 1;

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
INSERT INTO floor_events (id, order_id, type, payload, at, subject) VALUES (?, ?, ?, ?, ?, ?);

-- ListUnsyncedFloorEvents: oldest first, never a parked event nor one about
-- the same subject queued behind a parked one (they wait for its retry).
-- name: ListUnsyncedFloorEvents :many
SELECT * FROM floor_events f
WHERE f.synced_at IS NULL AND f.parked_at IS NULL
  AND (f.subject = '' OR NOT EXISTS (
        SELECT 1 FROM floor_events p
        WHERE p.subject = f.subject AND p.synced_at IS NULL AND p.rowid < f.rowid
          AND (p.parked_at IS NOT NULL OR p.refusals > 0)))
ORDER BY f.rowid ASC LIMIT ?;

-- name: MarkFloorEventRefused :one
UPDATE floor_events SET refusals = refusals + 1, attempts = attempts + 1, last_error = sqlc.arg(last_error),
       first_refused_at = COALESCE(first_refused_at, sqlc.arg(at))
WHERE id = sqlc.arg(id)
RETURNING refusals, first_refused_at, subject, type;

-- name: ParkFloorEvent :execrows
UPDATE floor_events SET parked_at = sqlc.arg(parked_at) WHERE id = sqlc.arg(id) AND synced_at IS NULL AND parked_at IS NULL;

-- name: InsertDeadLetter :exec
INSERT INTO floor_dead_letters (event_id, subject, type, result, refusals, first_refused_at, parked_at, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (event_id) DO UPDATE SET result = excluded.result, refusals = excluded.refusals,
  first_refused_at = excluded.first_refused_at, parked_at = excluded.parked_at, last_error = excluded.last_error,
  retried_at = NULL, retried_by = '';

-- name: ListOpenDeadLetters :many
SELECT d.event_id, d.subject, d.type, d.result, d.refusals, d.first_refused_at, d.parked_at, d.last_error, f.at
FROM floor_dead_letters d JOIN floor_events f ON f.id = d.event_id
WHERE d.retried_at IS NULL AND f.parked_at IS NOT NULL AND f.synced_at IS NULL
ORDER BY f.rowid ASC LIMIT ?;

-- name: CountParkedFloorEvents :one
SELECT count(*) FROM floor_events WHERE parked_at IS NOT NULL AND synced_at IS NULL;

-- CountHeldFloorEvents: unsynced events waiting behind a parked one about
-- the same subject.
-- name: CountHeldFloorEvents :one
SELECT count(*) FROM floor_events f
WHERE f.synced_at IS NULL AND f.parked_at IS NULL AND f.subject <> ''
  AND EXISTS (SELECT 1 FROM floor_events p
              WHERE p.subject = f.subject AND p.parked_at IS NOT NULL AND p.synced_at IS NULL AND p.rowid < f.rowid);

-- name: UnparkFloorEvent :execrows
UPDATE floor_events SET parked_at = NULL, refusals = 0, first_refused_at = NULL
WHERE id = ? AND parked_at IS NOT NULL AND synced_at IS NULL;

-- name: MarkDeadLetterRetried :exec
UPDATE floor_dead_letters SET retried_at = ?, retried_by = ? WHERE event_id = ? AND retried_at IS NULL;

-- name: ListParkedFloorEventIDs :many
SELECT id FROM floor_events WHERE parked_at IS NOT NULL AND synced_at IS NULL ORDER BY rowid;

-- name: GetSyncParkSettings :one
SELECT sync_park_refusals, sync_park_hours, sync_park_alerted_at FROM settings WHERE id = 1;

-- name: SetSyncParkSettings :exec
UPDATE settings SET sync_park_refusals = ?, sync_park_hours = ? WHERE id = 1;

-- name: SetSyncParkAlertedAt :exec
UPDATE settings SET sync_park_alerted_at = ? WHERE id = 1;

-- name: MarkFloorEventSynced :exec
UPDATE floor_events SET synced_at = ?, last_error = '' WHERE id = ?;

-- name: MarkFloorEventFailed :exec
UPDATE floor_events SET attempts = attempts + 1, last_error = ? WHERE id = ?;

-- name: CountUnsyncedFloorEvents :one
SELECT count(*) FROM floor_events WHERE synced_at IS NULL AND parked_at IS NULL;

-- name: DeleteOldSyncedFloorEvents :execrows
DELETE FROM floor_events WHERE synced_at IS NOT NULL AND synced_at < ?;

-- Paired devices (counter tablets) -------------------------------------------

-- name: CreatePairedDevice :exec
INSERT INTO paired_devices (id, name, key_hash, key_prefix, kind) VALUES (?, ?, ?, ?, ?);

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

-- name: SetPairedDeviceAppVersion :exec
UPDATE paired_devices SET app_version = ?, app_version_seen_at = ? WHERE id = ?;

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

-- E: ticket numbers (contract 2.10ae, 2026-11-14): one sequence in
-- counter_next (TakeCounterNumber), restarted by every Close batch.

-- name: RestartNumbers :exec
-- Close batch: the next order's number is the start again (0 = start).
UPDATE settings SET counter_next = 0, kitchen_next = 0 WHERE id = 1;

-- name: PlaceholderHeld :one
-- A future order's placeholder is held by another open future order.
SELECT COUNT(*) FROM orders
WHERE number_placeholder = 1 AND kitchen_number = sqlc.arg(number)
  AND state IN ('arrived', 'scheduled');

-- name: SetPlaceholderNumber :execrows
-- Gives a future order its placeholder (only while it has no number).
UPDATE orders SET kitchen_number = sqlc.arg(number), number_placeholder = 1
WHERE id = sqlc.arg(id) AND kitchen_number IS NULL;

-- name: SetOrderPlaceholderFlag :exec
-- A restore: the order's number is a placeholder.
UPDATE orders SET number_placeholder = sqlc.arg(placeholder) WHERE id = sqlc.arg(id);

-- name: SetLastPrepMinutes :exec
UPDATE settings SET last_prep_minutes = ? WHERE id = 1;

-- name: ClearProvisioning :exec
-- Unlink / definitive revoke: drop the store key AND the local password + hint
-- in one statement, back to the blank "paste a token" state.
UPDATE settings
SET store_api_key = '', ui_password_hash = '', ui_password_hint = '',
    secret_pushover_app_token = '', secret_pushover_user_key = '', secret_pushover_boss_key = '',
    secret_pushover_admin_devices = '',
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
    secret_pushover_admin_devices = ?,
    secret_otlp_endpoint = ?, secret_otlp_headers = ?,
    manager_username = ?, device_id = ?, ui_password_hash = ?, ui_password_hint = ?
WHERE id = 1;

-- name: GetLinkInfo :one
SELECT secret_pushover_app_token, secret_pushover_user_key, secret_pushover_boss_key,
       secret_pushover_admin_devices,
       secret_otlp_endpoint, secret_otlp_headers, manager_username, device_id
FROM settings WHERE id = 1;

-- name: SetPushoverSecrets :exec
-- The Pushover part of the link secrets, refreshed from main (2.3.7).
UPDATE settings
SET secret_pushover_app_token = ?, secret_pushover_user_key = ?, secret_pushover_boss_key = ?,
    secret_pushover_admin_devices = ?
WHERE id = 1;

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
                    pool_driver_id, pooled_at, pool_seq, pool_plan_id,
                    scheduled_lead_minutes, ring_at, rerung,
                    came_back, not_delivered_action, not_delivered_at, not_delivered_driver_id, run_seq,
                    added_tip_cents, tip_locked, cancel_reason,
                    paid, paid_at, tip_cents, client_key, door_tip_cents)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
        ?, ?, ?, ?, ?);

-- name: RestoreDriver :exec
INSERT INTO drivers (id, name, active, position, status, last_returned_at) VALUES (?, ?, true, ?, ?, ?);

-- name: RestoreShift :exec
INSERT INTO driver_shifts (id, driver_id, driver_name, batch_id, clock_in) VALUES (?, ?, ?, ?, ?);

-- name: SetKitchenCounters :exec
UPDATE settings SET kitchen_next = ?, last_prep_minutes = ? WHERE id = 1;

-- Pi updates (2.0.6) ----------------------------------------------------------

-- name: CountOpenBatchOrders :one
SELECT CAST(count(*) AS INTEGER) AS total,
       CAST(COALESCE(SUM(CASE WHEN state IN ('arrived', 'accepted', 'ready', 'on_delivery') AND finished_at IS NULL THEN 1 ELSE 0 END), 0) AS INTEGER) AS active
FROM orders
WHERE batch_id = (SELECT id FROM batches WHERE closed_at IS NULL ORDER BY id DESC LIMIT 1);

-- Metrics: floor.orders.open (open batch, by state and online / channel).
-- name: CountOpenFloorOrders :many
SELECT state,
       CAST(source NOT IN ('online', 'pos', '') AS INTEGER) AS channel,
       CAST(count(*) AS INTEGER) AS n
FROM orders
WHERE batch_id = (SELECT id FROM batches WHERE closed_at IS NULL ORDER BY id DESC LIMIT 1)
  AND state IN ('arrived', 'scheduled', 'accepted', 'ready', 'on_delivery')
GROUP BY state, channel;

-- name: GetUpdateState :one
SELECT update_job, update_notified FROM settings WHERE id = 1;

-- name: SetUpdateJob :exec
UPDATE settings SET update_job = ? WHERE id = 1;

-- name: SetUpdateNotified :exec
UPDATE settings SET update_notified = ? WHERE id = 1;

-- name: GetUpdateLastSuccess :one
SELECT update_last_success FROM settings WHERE id = 1;

-- name: SetUpdateLastSuccess :exec
UPDATE settings SET update_last_success = ? WHERE id = 1;

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
                       outside_area, store_to_s, to_store_s, updated_at, candidates, confirmed_address,
                       corrected, note)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (order_id) DO UPDATE SET
    state = excluded.state, lat = excluded.lat, lng = excluded.lng, quality = excluded.quality,
    distance_m = excluded.distance_m, duration_s = excluded.duration_s, polyline = excluded.polyline,
    outside_area = excluded.outside_area, store_to_s = excluded.store_to_s,
    to_store_s = excluded.to_store_s, updated_at = excluded.updated_at,
    candidates = excluded.candidates, confirmed_address = excluded.confirmed_address,
    corrected = excluded.corrected, note = excluded.note;

-- name: DeleteOrderGeo :exec
DELETE FROM order_geo WHERE order_id = ?;

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

-- name: GetLatestAutoPlan :one
-- Auto-assign waits while an AUTO preview may be on a tablet.
SELECT id, created_at, confirmed_at FROM auto_plans ORDER BY created_at DESC LIMIT 1;

-- name: DeleteOldAutoPlans :exec
DELETE FROM auto_plans WHERE created_at < ?;

-- Delivery settings + store map (2.0.9) -------------------------------------------

-- name: GetDeliveryPrefs :one
SELECT soon_ready_minutes, delivery_target_minutes, nearby_rule, nearby_detour_minutes, nearby_distance_mi, auto_assign
FROM settings WHERE id = 1;

-- name: SetDeliveryPrefs :exec
UPDATE settings SET soon_ready_minutes = ?, delivery_target_minutes = ?, nearby_rule = ?,
    nearby_detour_minutes = ?, nearby_distance_mi = ?, auto_assign = ?
WHERE id = 1;

-- name: GetAutoUpdate :one
SELECT auto_update FROM settings WHERE id = 1;

-- name: SetAutoUpdate :exec
UPDATE settings SET auto_update = ? WHERE id = 1;

-- name: GetMapFile :one
SELECT map_file FROM settings WHERE id = 1;

-- name: SetMapFile :exec
UPDATE settings SET map_file = ? WHERE id = 1;

-- name: SetOrderSnapshot :exec
UPDATE orders SET snapshot = ? WHERE id = ?;

-- Future orders (2.1.0) --------------------------------------------------------

-- name: ScheduleOrder :execrows
-- First confirm of a future order (or a lead edit): it waits until ring_at.
UPDATE orders SET state = 'scheduled', scheduled_lead_minutes = ?, ring_at = ?
WHERE id = ? AND state IN ('arrived', 'scheduled');

-- name: PrepareMakeNow :execrows
-- 2.4.3 MAKE NOW: a waiting future order leaves the future (made_now hides
-- its promised_at) and waits as a plain arrived order for the accept that
-- follows at once.
UPDATE orders SET made_now = true, state = 'arrived', rerung = false, ring_at = NULL
WHERE id = ? AND state IN ('arrived', 'scheduled') AND accept_pending = 0;

-- name: SetDueOverride :execrows
-- 2.4.3 CHANGE DUE TIME: the Pi's own due time (main keeps the vendor's);
-- a scheduled order's ring time moves with it.
UPDATE orders SET due_override = sqlc.arg(due), ring_at = COALESCE(sqlc.narg(ring_at), ring_at)
WHERE id = sqlc.arg(id) AND state IN ('arrived', 'scheduled');

-- name: CancelWaiting :execrows
-- 2.4.3 CANCEL on a waiting future order (never accepted, nothing cooked).
UPDATE orders SET state = 'cancelled', cancelled_at = ?, ring_at = NULL, pool_driver_id = NULL, pooled_at = NULL,
    pool_seq = NULL, pool_plan_id = NULL
WHERE id = ? AND state IN ('arrived', 'scheduled') AND accept_pending = 0;

-- name: ListDueScheduled :many
SELECT * FROM orders WHERE state = 'scheduled' AND ring_at <= ? ORDER BY ring_at ASC;

-- name: RaiseScheduled :execrows
-- ring_at passed: it rings again like a new order. last_alert_at starts the
-- 2-minute escalation from now, not from the first arrival.
UPDATE orders SET state = 'arrived', rerung = true, last_alert_at = ?
WHERE id = ? AND state = 'scheduled';

-- name: MoveScheduledToBatch :exec
-- Close batch: future orders wait in the next batch.
UPDATE orders SET batch_id = sqlc.arg(new_batch) WHERE batch_id = sqlc.arg(old_batch) AND state = 'scheduled';

-- name: MoveOrderToBatch :exec
-- Close batch: a future order still ringing (not accepted, promised_at ahead)
-- waits in the next batch too.
UPDATE orders SET batch_id = sqlc.arg(new_batch) WHERE id = sqlc.arg(id) AND batch_id = sqlc.arg(old_batch) AND state = 'arrived';

-- Integration feed (2.1.0) -----------------------------------------------------

-- name: InsertIntegrationEvent :exec
INSERT INTO integration_events (type, payload, created_at) VALUES (?, ?, ?);

-- name: ListIntegrationEvents :many
SELECT * FROM integration_events WHERE seq > ? ORDER BY seq ASC LIMIT ?;

-- name: MaxIntegrationSeq :one
SELECT CAST(COALESCE(MAX(seq), 0) AS INTEGER) AS max_seq FROM integration_events;

-- name: DeleteOldIntegrationEvents :exec
DELETE FROM integration_events WHERE created_at < ?;

-- Integration settings (2.1.0) ---------------------------------------------------

-- name: GetIntegrationSettings :one
SELECT features, ticket_toggles, default_pickup_minutes, default_delivery_minutes, finishing, callout, look FROM settings WHERE id = 1;

-- name: SetIntegrationSettings :exec
UPDATE settings SET features = ?, ticket_toggles = ?, default_pickup_minutes = ?, default_delivery_minutes = ?,
    finishing = ?, callout = ?, look = ?
WHERE id = 1;

-- name: GetIntegrationStatus :one
SELECT integration_status FROM settings WHERE id = 1;

-- name: SetIntegrationStatus :exec
UPDATE settings SET integration_status = ? WHERE id = 1;

-- Finishing orders, terminal changes (00019) -----------------------------------

-- name: SetOrderFinished :execrows
-- A ready pickup / counter order is done with (picked up): the POS closed its
-- check ('pos') or an online pickup sat ready for the store's finish time
-- ('auto'). It stays ready; it no longer counts as open.
UPDATE orders SET finished_at = sqlc.arg(finished_at), finished_by = sqlc.arg(finished_by)
WHERE id = sqlc.arg(id) AND state = 'ready' AND finished_at IS NULL;

-- name: ListDueOnlinePickups :many
-- Online pickups / dine-in ready since before the cutoff and not finished yet.
SELECT id FROM orders
WHERE state = 'ready' AND finished_at IS NULL AND ready_at IS NOT NULL AND ready_at <= sqlc.arg(cutoff)
  AND COALESCE(json_extract(snapshot, '$.fulfillment.type'), '') <> 'delivery';

-- name: InsertSettingsChange :exec
INSERT INTO settings_log (at, section, by_kind, by_name, device_id, changes)
VALUES (sqlc.arg(at), sqlc.arg(section), sqlc.arg(by_kind), sqlc.arg(by_name), sqlc.arg(device_id), sqlc.arg(changes));

-- name: PruneSettingsLog :exec
DELETE FROM settings_log WHERE at < sqlc.arg(before);

-- name: ListSettingsLog :many
-- Newest first, since a time (the keep window), at most lim rows.
SELECT * FROM settings_log WHERE at >= sqlc.arg(since) ORDER BY id DESC LIMIT sqlc.arg(lim);

-- name: SetAddedTip :execrows
-- 2.5.0 ADD TIP: the whole tip added after the order (absolute). Only an
-- order of the given (open) batch that isn't cancelled, POS or locked (0 rows).
UPDATE orders SET added_tip_cents = sqlc.arg(tip_cents)
WHERE id = sqlc.arg(id) AND NOT tip_locked AND state <> 'cancelled'
  AND batch_id = sqlc.arg(batch_id);

-- name: LockShiftTips :many
-- 2.5.0: a driver's shift ended - main charges the card deliveries they took
-- out during it, so their tips can't change any more. Returns the ids locked.
UPDATE orders SET tip_locked = true
WHERE driver_id = sqlc.arg(driver_id) AND NOT tip_locked
  AND state IN ('on_delivery', 'delivered')
  AND COALESCE(out_at, delivered_at, sqlc.arg(clock_in)) >= sqlc.arg(clock_in)
  AND batch_id = sqlc.arg(batch_id)
RETURNING id;

-- name: LockBatchTips :exec
-- 2.5.0 Close batch: main charges everything left, every tip is final.
UPDATE orders SET tip_locked = true WHERE batch_id = ? AND NOT tip_locked;

-- name: CancelChannelOrder :execrows
-- 2.5.0 CANCEL ORDER on a channel (website) order not delivered yet: off any
-- pool or run (no driver, so it counts on no driver report).
UPDATE orders
SET state = 'cancelled', cancelled_at = sqlc.arg(cancelled_at), cancel_reason = sqlc.arg(reason),
    ring_at = NULL, came_back = false,
    driver_id = NULL, driver_name = '', out_at = NULL, delivered_at = NULL, run_seq = NULL,
    pool_driver_id = NULL, pooled_at = NULL, pool_seq = NULL, pool_plan_id = NULL
WHERE id = sqlc.arg(id) AND accept_pending = 0
  AND state IN ('arrived', 'scheduled', 'accepted', 'ready', 'on_delivery');

-- ===== 00025 terminal (stage 0d): the Pi serves the store's POS terminals =====

-- name: GetTerminalSnapshot :one
SELECT * FROM terminal_snapshots WHERE kind = ?;

-- name: PutTerminalSnapshot :exec
-- The last good counter menu / terminal config main sent (replaced whole).
INSERT INTO terminal_snapshots (kind, version, etag, body, fetched_at)
VALUES (sqlc.arg(kind), sqlc.arg(version), sqlc.arg(etag), sqlc.arg(body), sqlc.arg(fetched_at))
ON CONFLICT (kind) DO UPDATE SET version = excluded.version, etag = excluded.etag,
    body = excluded.body, fetched_at = excluded.fetched_at;

-- name: TakeCounterNumber :one
-- The store's one ticket-number sequence (every order, contract 2.10ae):
-- hands out counter_next (or start when it is 0, i.e. after Close batch) and
-- moves it on. Returns the number AFTER the one taken.
UPDATE settings
SET counter_next = CASE WHEN counter_next > 0 THEN counter_next + 1 ELSE CAST(sqlc.arg(start) AS INTEGER) + 1 END
WHERE id = 1
RETURNING counter_next;

-- name: GetCounterNext :one
SELECT counter_next FROM settings WHERE id = 1;

-- name: SetCounterNext :exec
UPDATE settings SET counter_next = ? WHERE id = 1;


-- name: InsertCounterOrder :exec
-- A counter order from a terminal: stored accepted at once (no ring) with its
-- counter number, in the open batch.
INSERT INTO orders (id, tenant_id, location_id, source, source_event_id, snapshot, state, arrival_date,
                    batch_id, kitchen_number, prep_minutes, estimated_ready_at, accepted_at,
                    paid, cash_due_cents, tip_cents, sent_at, paid_at, client_key, request_hash, kitchen_pending)
VALUES (sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(location_id), sqlc.arg(source), sqlc.arg(source_event_id),
        sqlc.arg(snapshot), 'accepted', sqlc.arg(arrival_date),
        (SELECT id FROM batches WHERE closed_at IS NULL ORDER BY id DESC LIMIT 1),
        sqlc.arg(kitchen_number), sqlc.arg(prep_minutes), sqlc.arg(estimated_ready_at), sqlc.arg(accepted_at),
        sqlc.arg(paid), sqlc.arg(cash_due_cents), sqlc.arg(tip_cents), sqlc.arg(sent_at), sqlc.narg(paid_at),
        sqlc.arg(client_key), sqlc.arg(request_hash), sqlc.arg(kitchen_pending));

-- name: GetOrderByClientKey :one
SELECT * FROM orders WHERE client_key = ? AND client_key <> '';

-- name: SetCounterMoney :exec
-- A payment / refund on a counter order: the snapshot (payments, tip, total)
-- and the money columns.
UPDATE orders SET snapshot = sqlc.arg(snapshot), paid = sqlc.arg(paid), cash_due_cents = sqlc.arg(cash_due_cents),
    tip_cents = sqlc.arg(tip_cents), paid_at = sqlc.narg(paid_at)
WHERE id = sqlc.arg(id);

-- name: ListCounterOrders :many
-- Counter orders of a batch (the terminals' Pick Up list filters them).
SELECT * FROM orders WHERE source = 'counter' AND batch_id = ? ORDER BY arrival_date, id;

-- name: InsertOrderPayment :exec
INSERT INTO order_payments (id, order_id, client_key, at, tender, amount_cents, tip_cents, tendered_cents,
                            change_cents, refund, reason, ref, last4, staff_id, device_id, approved_by, drawer_id, brand,
                            refund_kind, request_hash, batch_id)
VALUES (sqlc.arg(id), sqlc.arg(order_id), sqlc.arg(client_key), sqlc.arg(at), sqlc.arg(tender), sqlc.arg(amount_cents),
        sqlc.arg(tip_cents), sqlc.arg(tendered_cents), sqlc.arg(change_cents), sqlc.arg(refund), sqlc.arg(reason),
        sqlc.arg(ref), sqlc.arg(last4), sqlc.arg(staff_id), sqlc.arg(device_id), sqlc.arg(approved_by), sqlc.arg(drawer_id),
        sqlc.arg(brand), sqlc.arg(refund_kind), sqlc.arg(request_hash),
        (SELECT id FROM batches WHERE closed_at IS NULL ORDER BY id DESC LIMIT 1));

-- name: ListOrderPayments :many
SELECT * FROM order_payments WHERE order_id = ? ORDER BY at, rowid;

-- name: GetOrderPaymentByKey :one
SELECT * FROM order_payments WHERE order_id = ? AND client_key = ? AND client_key <> '';

-- name: InsertJournal :exec
INSERT INTO journal (id, at, staff_id, device_id, action, order_id, detail)
VALUES (sqlc.arg(id), sqlc.arg(at), sqlc.arg(staff_id), sqlc.arg(device_id), sqlc.arg(action), sqlc.arg(order_id), sqlc.arg(detail));

-- name: ListJournal :many
-- Newest first.
SELECT * FROM journal ORDER BY at DESC, rowid DESC LIMIT ?;

-- name: DeleteOrphanOrderPayments :exec
-- Retention: the payments of trimmed counter orders.
DELETE FROM order_payments WHERE order_id NOT IN (SELECT id FROM orders);

-- name: PruneJournal :execrows
-- The Pi keeps the journal 30 days (main keeps it all).
DELETE FROM journal WHERE at < ?;

-- Terminal driver view + batch auto-close (00026_terminal_driver) ----------------

-- name: GetTerminalDriverLink :one
SELECT driver_id FROM terminal_driver_links WHERE staff_id = ?;

-- name: ListTerminalDriverLinks :many
SELECT staff_id, driver_id FROM terminal_driver_links ORDER BY staff_id;

-- name: DeleteTerminalDriverLinkOfDriver :exec
-- A driver is linked to one staff member at a time.
DELETE FROM terminal_driver_links WHERE driver_id = sqlc.arg(driver_id) AND staff_id <> sqlc.arg(staff_id);

-- name: UpsertTerminalDriverLink :exec
INSERT INTO terminal_driver_links (staff_id, driver_id, linked_at) VALUES (?, ?, ?)
ON CONFLICT (staff_id) DO UPDATE SET driver_id = excluded.driver_id, linked_at = excluded.linked_at;

-- name: GetStaffOfDriver :one
-- The staff member a Pi driver is (main's Store > Staff, contract 2.1f).
SELECT staff_id FROM terminal_driver_links WHERE driver_id = ?;

-- name: ReactivateDriver :execrows
-- A staff driver switched back on in main: on the roster again, clocked out.
UPDATE drivers SET active = true, status = 'off' WHERE id = ? AND NOT active;

-- name: DeactivateOffDriver :execrows
-- A staff driver switched off in main leaves the roster once he is off.
UPDATE drivers SET active = false WHERE id = ? AND active AND status = 'off';

-- name: GetAutoCloseLast :one
SELECT auto_close_last FROM settings WHERE id = 1;

-- name: SetAutoCloseLast :exec
UPDATE settings SET auto_close_last = ? WHERE id = 1;

-- The terminals' shared drafts (00027, contract 2.10i) -------------------------

-- name: GetTerminalDraft :one
SELECT * FROM terminal_drafts WHERE id = ?;

-- name: ListTerminalDrafts :many
SELECT * FROM terminal_drafts WHERE location_id = ? ORDER BY created_at, id;

-- name: CountTerminalDrafts :one
SELECT CAST(count(*) AS INTEGER) FROM terminal_drafts WHERE location_id = ?;

-- name: UpsertTerminalDraft :exec
INSERT INTO terminal_drafts (id, location_id, type, name, held, holder_device_id, holder_device_name, holder_staff_id,
                             holder_staff_name, staff_id, staff_name, order_id, draft, created_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(location_id), sqlc.arg(type), sqlc.arg(name), sqlc.arg(held), sqlc.arg(holder_device_id),
        sqlc.arg(holder_device_name), sqlc.arg(holder_staff_id), sqlc.arg(holder_staff_name), sqlc.arg(staff_id),
        sqlc.arg(staff_name), sqlc.arg(order_id), sqlc.arg(draft), sqlc.arg(created_at), sqlc.arg(updated_at))
ON CONFLICT (id) DO UPDATE SET
    type = excluded.type, name = excluded.name, held = excluded.held, holder_device_id = excluded.holder_device_id,
    holder_device_name = excluded.holder_device_name, holder_staff_id = excluded.holder_staff_id,
    holder_staff_name = excluded.holder_staff_name, staff_id = excluded.staff_id, staff_name = excluded.staff_name,
    order_id = excluded.order_id, draft = excluded.draft, updated_at = excluded.updated_at;

-- name: DeleteTerminalDraft :execrows
DELETE FROM terminal_drafts WHERE id = ?;

-- name: ClearTerminalDrafts :many
-- Close batch: every shared draft goes (their ids are announced).
DELETE FROM terminal_drafts RETURNING id;

-- name: ListPickupOrders :many
-- The terminals' Pickup list (contract 2.10i): a batch's pickup / counter orders of
-- every source, oldest first (the service filters states).
SELECT * FROM orders
WHERE batch_id = ?
  AND COALESCE(json_extract(snapshot, '$.fulfillment.type'), '') <> 'delivery'
ORDER BY arrival_date, id;

-- name: SetTerminalPaidMoney :exec
-- A payment taken at a terminal on an online pickup (contract 2.10i): the
-- money columns only; the snapshot stays main's.
UPDATE orders SET paid = sqlc.arg(paid), cash_due_cents = sqlc.arg(cash_due_cents), tip_cents = sqlc.arg(tip_cents),
    paid_at = sqlc.narg(paid_at)
WHERE id = sqlc.arg(id);

-- 00028_customers: the store's customers (main's feed + the terminals).

-- name: GetCustomerSync :one
SELECT location_id, cursor, synced_at FROM customer_sync WHERE id = 1;

-- name: PutCustomerSync :exec
INSERT INTO customer_sync (id, location_id, cursor, synced_at) VALUES (1, sqlc.arg(location_id), sqlc.arg(cursor), sqlc.arg(synced_at))
ON CONFLICT (id) DO UPDATE SET location_id = excluded.location_id, cursor = excluded.cursor, synced_at = excluded.synced_at;

-- name: GetCustomer :one
SELECT * FROM customers WHERE id = ?;

-- name: GetCustomerByPhone :one
SELECT * FROM customers WHERE phone = ?;

-- name: UpsertCustomer :exec
-- last_order_at is the Pi's own: never written here.
INSERT INTO customers (id, phone, phone_rev, first_name, last_name, name, notes, source, updated_at, local_at)
VALUES (sqlc.arg(id), sqlc.arg(phone), sqlc.arg(phone_rev), sqlc.arg(first_name), sqlc.arg(last_name), sqlc.arg(name),
        sqlc.arg(notes), sqlc.arg(source), sqlc.arg(updated_at), sqlc.arg(local_at))
ON CONFLICT (id) DO UPDATE SET phone = excluded.phone, phone_rev = excluded.phone_rev, first_name = excluded.first_name,
    last_name = excluded.last_name, name = excluded.name, notes = excluded.notes, source = excluded.source,
    updated_at = excluded.updated_at, local_at = excluded.local_at;

-- name: DeleteCustomer :exec
DELETE FROM customers WHERE id = ?;

-- name: DeleteCustomerAddresses :exec
DELETE FROM customer_addresses WHERE customer_id = ?;

-- name: DeleteAllCustomers :exec
DELETE FROM customers;

-- name: DeleteAllCustomerAddresses :exec
DELETE FROM customer_addresses;

-- name: DeleteSyncedCustomers :exec
-- A feed reset: drop main's copy, keep what a terminal changed (on its way to main).
DELETE FROM customers WHERE local_at = '';

-- name: DeleteOrphanCustomerAddresses :exec
DELETE FROM customer_addresses WHERE customer_id NOT IN (SELECT id FROM customers);

-- name: ListCustomerAddresses :many
SELECT * FROM customer_addresses WHERE customer_id = ? ORDER BY position, created_at, id;

-- name: InsertCustomerAddress :exec
INSERT INTO customer_addresses (id, customer_id, position, label, line1, line2, city, state, zip, notes, is_default, created_at, lat, lng)
VALUES (sqlc.arg(id), sqlc.arg(customer_id), sqlc.arg(position), sqlc.arg(label), sqlc.arg(line1), sqlc.arg(line2), sqlc.arg(city),
        sqlc.arg(state), sqlc.arg(zip), sqlc.arg(notes), sqlc.arg(is_default), sqlc.arg(created_at), sqlc.arg(lat), sqlc.arg(lng));

-- name: SetCustomerAddressPoint :exec
UPDATE customer_addresses SET lat = sqlc.arg(lat), lng = sqlc.arg(lng) WHERE id = sqlc.arg(id);

-- name: FindCustomersByPhonePrefix :many
-- phone in [lo, hi): the typed digits as a prefix (index idx_customers_phone).
SELECT * FROM customers WHERE phone >= sqlc.arg(lo) AND phone < sqlc.arg(hi)
ORDER BY last_order_at DESC, updated_at DESC, id LIMIT sqlc.arg(max_rows);

-- name: FindCustomersByPhoneSuffix :many
-- phone_rev in [lo, hi): the typed digits as the number's end ("the last 4").
SELECT * FROM customers WHERE phone_rev >= sqlc.arg(lo) AND phone_rev < sqlc.arg(hi)
ORDER BY last_order_at DESC, updated_at DESC, id LIMIT sqlc.arg(max_rows);

-- name: TouchCustomerOrder :exec
UPDATE customers SET last_order_at = sqlc.arg(at)
WHERE (id = sqlc.arg(id) OR phone = sqlc.arg(phone)) AND last_order_at < sqlc.arg(at);

-- name: CountCustomers :one
SELECT count(*) FROM customers;

-- name: ListOrdersMentioning :many
-- Orders whose snapshot holds the text (a phone's last digits, a customer
-- id), newest first; the caller checks each one.
SELECT * FROM orders WHERE instr(snapshot, sqlc.arg(text)) > 0
ORDER BY arrival_date DESC LIMIT sqlc.arg(max_rows);

-- name: ListLocatedDeliveries :many
-- Delivery orders main located (their address + point), for the customers'
-- in / out of area check.
SELECT o.snapshot, g.lat, g.lng FROM orders o JOIN order_geo g ON g.order_id = o.id
WHERE g.state = 'located' AND g.lat IS NOT NULL AND g.lng IS NOT NULL
  AND json_extract(o.snapshot, '$.fulfillment.type') = 'delivery'
ORDER BY o.arrival_date DESC LIMIT 2000;

-- Cash drawers (00029_drawers_z) ---------------------------------------------

-- name: InsertDrawerSession :exec
INSERT INTO drawer_sessions (id, device_id, device_name, staff_id, staff_name, batch_id, opened_at, start_cents, handover_from, auto)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetPairedDeviceName :one
SELECT name FROM paired_devices WHERE id = ?;

-- name: GetDrawerSession :one
SELECT * FROM drawer_sessions WHERE id = ?;

-- name: GetOpenDrawerByDevice :one
SELECT * FROM drawer_sessions WHERE device_id = ? AND closed_at IS NULL;

-- name: GetOpenDrawerByStaff :one
SELECT * FROM drawer_sessions WHERE staff_id = ? AND closed_at IS NULL;

-- name: ListOpenDrawers :many
SELECT * FROM drawer_sessions WHERE closed_at IS NULL ORDER BY opened_at;

-- name: ListDrawersByBatch :many
SELECT * FROM drawer_sessions WHERE batch_id = ? ORDER BY opened_at, rowid;

-- name: CloseDrawerSession :execrows
UPDATE drawer_sessions SET closed_at = sqlc.arg(closed_at), closed_by = sqlc.arg(closed_by), close_reason = sqlc.arg(close_reason),
       counted_cents = sqlc.arg(counted_cents), denominations = sqlc.arg(denominations), expected_cents = sqlc.arg(expected_cents),
       flagged = sqlc.arg(flagged)
WHERE id = sqlc.arg(id) AND closed_at IS NULL;

-- name: InsertDrawerMovement :exec
INSERT INTO drawer_movements (id, drawer_id, kind, amount_cents, reason, staff_id, approved_by, client_key, at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetDrawerMovementByKey :one
SELECT * FROM drawer_movements WHERE drawer_id = ? AND client_key = ? AND client_key <> '';

-- name: ListDrawerMovements :many
SELECT * FROM drawer_movements WHERE drawer_id = ? ORDER BY at, rowid;

-- name: SumDrawerCash :one
SELECT CAST(COALESCE(SUM(CASE WHEN refund THEN 0 ELSE amount_cents END), 0) AS INTEGER) AS sales_cents,
       CAST(COALESCE(SUM(CASE WHEN refund THEN 0 ELSE tip_cents END), 0) AS INTEGER) AS tips_cents,
       CAST(COALESCE(SUM(CASE WHEN refund THEN amount_cents + tip_cents ELSE 0 END), 0) AS INTEGER) AS refunds_cents,
       count(*) AS payments
FROM order_payments WHERE drawer_id = ?;

-- name: SumDrawerMovements :one
SELECT CAST(COALESCE(SUM(CASE WHEN kind = 'pay_in' THEN amount_cents ELSE 0 END), 0) AS INTEGER) AS pay_ins_cents,
       CAST(COALESCE(SUM(CASE WHEN kind = 'pay_out' THEN amount_cents ELSE 0 END), 0) AS INTEGER) AS pay_outs_cents
FROM drawer_movements WHERE drawer_id = ?;

-- CountNoSales: a terminal's no_sale journal rows in a time window
-- (closed_at '' = still open).
-- name: CountNoSales :one
SELECT count(*) FROM journal
WHERE action = 'no_sale' AND device_id = sqlc.arg(device_id) AND at >= sqlc.arg(from_at)
  AND (sqlc.arg(to_at) = '' OR at <= sqlc.arg(to_at));

-- Z reports -------------------------------------------------------------------

-- name: InsertPendingZReport :exec
INSERT INTO z_reports (batch_id, id, status, close_kind, forced, created_at) VALUES (?, ?, 'pending', ?, ?, ?)
ON CONFLICT (batch_id) DO NOTHING;

-- name: GetZReport :one
SELECT * FROM z_reports WHERE batch_id = ?;

-- name: ListPendingZReports :many
SELECT * FROM z_reports WHERE status = 'pending' ORDER BY batch_id;

-- name: ListZReports :many
SELECT * FROM z_reports ORDER BY batch_id DESC LIMIT ?;

-- name: SaveZReport :exec
UPDATE z_reports SET status = 'done', report = ?, built_at = ? WHERE batch_id = ?;

-- name: ListBatchPayments :many
-- The payments taken in a batch (00033: a carried order's later payment
-- counts on the batch it was taken in; an older row has its order's batch).
SELECT p.* FROM order_payments p WHERE p.batch_id = ? ORDER BY p.at, p.rowid;

-- name: ListJournalBetween :many
SELECT * FROM journal WHERE at >= sqlc.arg(from_at) AND at <= sqlc.arg(to_at) ORDER BY at, rowid;

-- name: ListAllBatchOrders :many
SELECT * FROM orders WHERE batch_id = ? ORDER BY arrival_date, rowid;

-- Retention of stage 2 rows: drawers and Z reports of trimmed batches, dead
-- letters whose event is gone.
-- name: DeleteOrphanDrawerMovements :exec
DELETE FROM drawer_movements WHERE drawer_id IN (SELECT id FROM drawer_sessions WHERE batch_id NOT IN (SELECT id FROM batches));

-- name: DeleteOrphanDrawerSessions :exec
DELETE FROM drawer_sessions WHERE batch_id NOT IN (SELECT id FROM batches);

-- name: DeleteOrphanZReports :exec
DELETE FROM z_reports WHERE status = 'done' AND batch_id NOT IN (SELECT id FROM batches);

-- name: DeleteOrphanDeadLetters :exec
DELETE FROM floor_dead_letters WHERE event_id NOT IN (SELECT id FROM floor_events);

-- Hardening round (00033_hardening, contract 2.10s) ------------------------------

-- name: MarkFloorEventsSyncedAt :exec
-- One flush's events in one statement (S-7: one fsync, not one per event).
UPDATE floor_events SET synced_at = sqlc.arg(synced_at), last_error = '' WHERE id IN (sqlc.slice(ids));

-- name: OldestUnsyncedByType :many
-- CM-9: the oldest unsynced, unparked event of each type.
SELECT type, CAST(min(at) AS TEXT) AS oldest FROM floor_events
WHERE synced_at IS NULL AND parked_at IS NULL GROUP BY type;

-- name: SetOrderCarried :exec
UPDATE orders SET carried = sqlc.arg(carried) WHERE id = sqlc.arg(id);

-- name: ListCarriedOrders :many
-- Unsettled checks carried past their batch's close (M-10), oldest first.
SELECT * FROM orders WHERE carried ORDER BY arrival_date, id;

-- name: SetKitchenPending :exec
UPDATE orders SET kitchen_pending = sqlc.arg(pending) WHERE id = sqlc.arg(id);

-- name: ListKitchenPending :many
-- M-11: counter orders whose kitchen tickets never printed (a restart in between).
SELECT * FROM orders WHERE kitchen_pending ORDER BY arrival_date, id;

-- name: SetZReportFlagged :exec
UPDATE z_reports SET flagged = true WHERE batch_id = ?;

-- name: ListFlaggedZReports :many
SELECT batch_id, id, built_at FROM z_reports WHERE flagged ORDER BY batch_id DESC LIMIT ?;

-- name: GetPinThrottle :one
SELECT pin_throttle FROM settings WHERE id = 1;

-- name: SetPinThrottle :exec
UPDATE settings SET pin_throttle = ? WHERE id = 1;

-- name: GetJournalByClientKey :one
-- A terminal's journal row its offline replay sent before (00034).
SELECT * FROM journal
WHERE device_id = sqlc.arg(device_id) AND action = sqlc.arg(action)
  AND json_extract(detail, '$.client_key') = sqlc.arg(client_key)
LIMIT 1;

-- name: SetReadyAt :execrows
-- A terminal moved an order's ready / delivery time (contract 2.10v): the
-- estimate and the prep minutes it implies, while the order is cooking or ready.
UPDATE orders
SET estimated_ready_at = sqlc.arg(estimated_ready_at),
    prep_minutes       = sqlc.arg(prep_minutes)
WHERE id = sqlc.arg(id) AND state IN ('accepted', 'ready');

-- name: GetTerminalAppRelease :one
SELECT terminal_app_release FROM settings WHERE id = 1;

-- name: SetTerminalAppRelease :exec
UPDATE settings SET terminal_app_release = ? WHERE id = 1;

-- B: reopen a check (00045, contract 2.10ak) ----------------------------------

-- name: SetOrderReopened :execrows
-- A cashier reopened the check from All checks: open again until something
-- settles it (paid_at / finished_at / out_at / delivered_at after this).
UPDATE orders SET reopened_at = sqlc.arg(reopened_at) WHERE id = sqlc.arg(id);

-- name: SetOrderRefinished :execrows
-- A reopened check picked up again (contract 2.10ak): finished_at moves on.
UPDATE orders SET finished_at = sqlc.arg(finished_at), finished_by = sqlc.arg(finished_by)
WHERE id = sqlc.arg(id) AND state = 'ready';
