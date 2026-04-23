-- name: UpsertArrivedOrder :exec
-- Inserts a freshly-arrived order. Idempotent against duplicate SSE redelivery —
-- if the order_id already exists (e.g. mobile reconnected and main/ resends),
-- we leave the existing row (and its state/printed_date) alone.
INSERT INTO orders (order_id, payload, state, arrival_date)
VALUES ($1, $2, 'arrived', now())
ON CONFLICT (order_id) DO NOTHING;

-- name: GetOrderByID :one
SELECT order_id, payload, state, arrival_date, printed_date
FROM orders
WHERE order_id = $1;

-- name: GetOrderByIDForUpdate :one
-- Row-level lock so the accept path is serialized against concurrent calls
-- (e.g. mobile double-tap). Used inside the accept transaction.
SELECT order_id, payload, state, arrival_date, printed_date
FROM orders
WHERE order_id = $1
FOR UPDATE;

-- name: ListOrdersPage :many
-- Newest-first, for mobile history view.
SELECT order_id, payload, state, arrival_date, printed_date
FROM orders
ORDER BY arrival_date DESC
LIMIT $1 OFFSET $2;

-- name: ListArrivedOrders :many
-- Pending-acceptance queue. Used by mobile to populate the "awaiting you" list
-- when it connects or reconnects to the SSE stream.
SELECT order_id, payload, state, arrival_date, printed_date
FROM orders
WHERE state = 'arrived'
ORDER BY arrival_date ASC;

-- name: MarkAccepted :exec
-- First-time accept: sets state + printed_date. COALESCE preserves any earlier
-- printed_date (so this is safe even if called twice, though the state check
-- below ordinarily prevents that).
UPDATE orders
SET state        = 'accepted',
    printed_date = COALESCE(printed_date, now())
WHERE order_id = $1;

-- name: DeleteOlderThan :execrows
DELETE FROM orders
WHERE arrival_date < $1;

-- name: GetAutoAccept :one
SELECT auto_accept FROM settings WHERE id = 1;

-- name: SetAutoAccept :exec
INSERT INTO settings (id, auto_accept, updated_at)
VALUES (1, $1, now())
ON CONFLICT (id) DO UPDATE SET
    auto_accept = EXCLUDED.auto_accept,
    updated_at  = EXCLUDED.updated_at;

-- name: UpsertPrinterConfigIfAbsent :exec
-- Env-var seed path: only populate a row if none exists for this printer.
-- Mobile-set values are never overwritten at boot.
INSERT INTO printer_configs (name, ip)
VALUES ($1, $2)
ON CONFLICT (name) DO NOTHING;

-- name: SetPrinterIP :exec
-- Mobile-update path: always overwrite. updated_at moves so mobile can tell
-- how fresh the value is.
INSERT INTO printer_configs (name, ip, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (name) DO UPDATE SET
    ip         = EXCLUDED.ip,
    updated_at = EXCLUDED.updated_at;

-- name: GetPrinterConfig :one
SELECT name, ip, updated_at FROM printer_configs WHERE name = $1;

-- name: ListPrinterConfigs :many
SELECT name, ip, updated_at FROM printer_configs ORDER BY name;
