-- SQLite schema (pure-Go modernc.org/sqlite). Mirrors the goose migrations in
-- internal/migrate/sql/. Timestamps are stored as RFC3339 text so the driver
-- never has to guess a time format; the repos parse them back into time.Time.
-- 00004_floor: batches (exactly one open), orders rebuilt with floor columns.
CREATE TABLE batches (
    id        integer PRIMARY KEY AUTOINCREMENT,
    opened_at text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    closed_at text
);

CREATE TABLE orders (
    id                 text    PRIMARY KEY,
    tenant_id          text    NOT NULL,
    location_id        text    NOT NULL,
    source             text    NOT NULL,
    source_event_id    text    NOT NULL,
    snapshot           text    NOT NULL,
    state              text    NOT NULL DEFAULT 'arrived',
    arrival_date       text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    printed_date       text,
    batch_id           integer NOT NULL,
    kitchen_number     integer,
    prep_minutes       integer,
    estimated_ready_at text,
    accepted_at        text,
    ready_at           text,
    out_at             text,
    delivered_at       text,
    driver_id          text,
    driver_name        text    NOT NULL DEFAULT '',
    last_alert_at      text,
    -- 00005_cancelled: never-accepted orders cancelled by Close batch.
    cancelled_at       text,
    -- 00009_accept_pending: claimed for accept, not yet recorded.
    accept_pending     integer NOT NULL DEFAULT 0,
    -- 00012_pools_geo: the available driver whose pool holds this delivery.
    pool_driver_id     text,
    pooled_at          text,
    -- 00013_pool_seq: stop order inside the pool (1..n).
    pool_seq           integer,
    pool_plan_id       text,
    -- 00014_integration: future orders + the money a terminal took (paid, cash due,
    -- tip, sent_at); the POS columns went in 00030_softtouch_out.
    paid                   boolean NOT NULL DEFAULT false,
    cash_due_cents         integer,
    tip_cents              integer,
    sent_at                text,
    scheduled_lead_minutes integer,
    ring_at                text,
    rerung                 boolean NOT NULL DEFAULT false,
    -- 00016_v220: accepted by auto-accept.
    auto_accepted          boolean NOT NULL DEFAULT false,
    -- 00017_not_delivered: came back with its driver (2.2.1).
    came_back               boolean NOT NULL DEFAULT false,
    not_delivered_action    text    NOT NULL DEFAULT '',
    not_delivered_at        text,
    not_delivered_driver_id text    NOT NULL DEFAULT '',
    run_seq                 integer,
    -- 00019_callout: finished (picked up) + by whom (terminal | auto).
    finished_at             text,
    finished_by             text    NOT NULL DEFAULT '',
    -- 00022_floor_24: delivered with no driver (Unassigned; JSON) and when
    -- the order was last paid.
    marked_delivered        text    NOT NULL DEFAULT '',
    paid_at                 text,
    -- 00022: a future order's tickets printed early (first ACCEPT).
    early_kitchen           boolean NOT NULL DEFAULT false,
    early_receipt           boolean NOT NULL DEFAULT false,
    -- 00023: a future order's due time moved on the Pi / MADE NOW.
    due_override            text,
    made_now                boolean NOT NULL DEFAULT false,
    -- 00024_weborder_floor: the tip added after the order (card channel
    -- orders), locked once main charged it; why it was cancelled by hand.
    added_tip_cents         integer NOT NULL DEFAULT 0,
    tip_locked              boolean NOT NULL DEFAULT false,
    cancel_reason           text    NOT NULL DEFAULT '',
    -- 00025_terminal: a counter order's terminal idempotency key ('' = none).
    client_key              text    NOT NULL DEFAULT '',
    -- 00026_terminal_driver: a tip the driver got at the door (cash / counter).
    door_tip_cents          integer NOT NULL DEFAULT 0,
    -- 00033_hardening: a keyed request's fingerprint (409 key_reused), a check
    -- carried past its batch's close, kitchen tickets still to print.
    request_hash            text    NOT NULL DEFAULT '',
    carried                 boolean NOT NULL DEFAULT false,
    kitchen_pending         boolean NOT NULL DEFAULT false,
    CHECK (state IN ('arrived', 'scheduled', 'accepted', 'ready', 'on_delivery', 'delivered', 'cancelled'))
);
CREATE INDEX idx_orders_arrival_date ON orders (arrival_date DESC);
CREATE INDEX idx_orders_state_pending ON orders (arrival_date DESC) WHERE state = 'arrived';
CREATE INDEX idx_orders_source_event ON orders (source, source_event_id);
CREATE INDEX idx_orders_batch ON orders (batch_id, arrival_date DESC);
CREATE INDEX idx_orders_pool ON orders (pool_driver_id) WHERE pool_driver_id IS NOT NULL;
CREATE INDEX idx_orders_scheduled ON orders (ring_at) WHERE state = 'scheduled';
CREATE UNIQUE INDEX idx_orders_client_key ON orders (client_key) WHERE client_key <> '';

CREATE TABLE drivers (
    id               text    PRIMARY KEY,
    name             text    NOT NULL,
    active           boolean NOT NULL DEFAULT true,
    position         integer NOT NULL DEFAULT 0,
    -- 00006_driver_shifts: 'off' = clocked out (not in the queue).
    status           text    NOT NULL DEFAULT 'off',
    last_returned_at text,
    created_at       text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- 00020_driver_route: the driver's phone in Pushover ('' = none).
    pushover_device  text    NOT NULL DEFAULT '',
    CHECK (status IN ('off', 'available', 'out'))
);

CREATE TABLE driver_shifts (
    id          text    PRIMARY KEY,
    driver_id   text    NOT NULL,
    driver_name text    NOT NULL,
    batch_id    integer NOT NULL,
    clock_in    text    NOT NULL,
    clock_out   text,
    auto_out    boolean NOT NULL DEFAULT false
);
CREATE INDEX idx_driver_shifts_open ON driver_shifts (driver_id) WHERE clock_out IS NULL;

CREATE TABLE floor_events (
    id         text    PRIMARY KEY,
    order_id   text,
    type       text    NOT NULL,
    payload    text    NOT NULL,
    at         text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    synced_at  text,
    attempts   integer NOT NULL DEFAULT 0,
    last_error text    NOT NULL DEFAULT '',
    -- 00029_drawers_z: the subject the event is about (order id,
    -- "customer:<id>", "drawer:<id>", "z:<batch>"; '' = none), main's
    -- refusals of a kept event and when it was parked (dead letter).
    subject          text    NOT NULL DEFAULT '',
    refusals         integer NOT NULL DEFAULT 0,
    first_refused_at text,
    parked_at        text
);
CREATE INDEX idx_floor_events_unsynced ON floor_events (at) WHERE synced_at IS NULL;
CREATE INDEX idx_floor_events_parked ON floor_events (subject) WHERE parked_at IS NOT NULL AND synced_at IS NULL;

-- 00029_drawers_z: why an event was parked, and its retry.
CREATE TABLE floor_dead_letters (
    event_id         text    PRIMARY KEY,
    subject          text    NOT NULL DEFAULT '',
    type             text    NOT NULL,
    result           text    NOT NULL DEFAULT '',
    refusals         integer NOT NULL DEFAULT 0,
    first_refused_at text    NOT NULL DEFAULT '',
    parked_at        text    NOT NULL,
    last_error       text    NOT NULL DEFAULT '',
    retried_at       text,
    retried_by       text    NOT NULL DEFAULT ''
);
CREATE INDEX idx_floor_dead_letters_open ON floor_dead_letters (parked_at) WHERE retried_at IS NULL;

CREATE TABLE paired_devices (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    key_hash     text NOT NULL UNIQUE,
    key_prefix   text NOT NULL,
    created_at   text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at text,
    revoked_at   text,
    -- 00014_integration: tablet | integration (the store hub); 00025_terminal: | terminal.
    kind         text NOT NULL DEFAULT 'tablet' CHECK (kind IN ('tablet', 'integration', 'terminal')),
    -- 00016_v220: the Quibase version the tablet last sent (X-Quibase-Version).
    app_version         text NOT NULL DEFAULT '',
    app_version_seen_at text
);

-- 00014_integration: the hub's cursor feed (kept 3 days).
CREATE TABLE integration_events (
    seq        integer PRIMARY KEY AUTOINCREMENT,
    type       text    NOT NULL,
    payload    text    NOT NULL,
    created_at text    NOT NULL
);
CREATE INDEX idx_integration_events_created ON integration_events (created_at);

CREATE TABLE settings (
    id            integer PRIMARY KEY DEFAULT 1,
    auto_accept   boolean NOT NULL DEFAULT false,
    store_api_key text    NOT NULL DEFAULT '',
    updated_at    text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- 00002_session_secret: persisted web-UI session-signing secret (hex).
    session_secret   text NOT NULL DEFAULT '',
    -- 00003_ui_password: local web-UI password (bcrypt) + optional hint.
    ui_password_hash text NOT NULL DEFAULT '',
    ui_password_hint text NOT NULL DEFAULT '',
    -- 00004_floor: kitchen-number counter + the last prep estimate picked.
    kitchen_next      integer NOT NULL DEFAULT 1100,
    last_prep_minutes integer NOT NULL DEFAULT 20,
    -- 00006_driver_shifts: daily driver clock-out time (location-local HH:MM).
    driver_reset_time    text NOT NULL DEFAULT '10:00',
    last_driver_reset_at text NOT NULL DEFAULT '',
    -- 00007_floor_settings: ticket lateness + driver-report tip tax, the last
    -- driver return (undo), and the cached tablet app release (JSON).
    green_till_minutes    integer NOT NULL DEFAULT 20,
    tip_tax_bps           integer NOT NULL DEFAULT 600,
    last_return_driver_id text    NOT NULL DEFAULT '',
    last_return_position  integer NOT NULL DEFAULT 0,
    last_return_at        text    NOT NULL DEFAULT '',
    app_release           text    NOT NULL DEFAULT '',
    -- 00008_link_restore: secrets from main (manager-login link), the manager
    -- username, this agent's device id, the last nightly backup upload.
    secret_pushover_app_token text NOT NULL DEFAULT '',
    secret_pushover_user_key  text NOT NULL DEFAULT '',
    secret_pushover_boss_key  text NOT NULL DEFAULT '',
    -- 00020_driver_route: the admin's Pushover devices (comma list).
    secret_pushover_admin_devices text NOT NULL DEFAULT '',
    secret_otlp_endpoint      text NOT NULL DEFAULT '',
    secret_otlp_headers       text NOT NULL DEFAULT '',
    manager_username          text NOT NULL DEFAULT '',
    device_id                 text NOT NULL DEFAULT '',
    last_backup_at            text NOT NULL DEFAULT '',
    -- 00010_receipt_prefs: receipt settings (JSON; missing keys = defaults).
    receipt_prefs             text NOT NULL DEFAULT '{}',
    -- 00011_updates_printer_macs: the latest Pi update request (JSON) + the
    -- "<id>:<state>" already announced by Pushover.
    update_job                text NOT NULL DEFAULT '',
    update_notified           text NOT NULL DEFAULT '',
    -- 00012_pools_geo: AUTO delivery settings + the downloaded store map (JSON).
    soon_ready_minutes        integer NOT NULL DEFAULT 5,
    delivery_target_minutes   integer NOT NULL DEFAULT 45,
    map_file                  text NOT NULL DEFAULT '',
    -- 00014_integration: feature toggles + kitchen-ticket switches (JSON),
    -- default pickup / delivery minutes, the hub's last status (JSON).
    features                  text NOT NULL DEFAULT '{}',
    ticket_toggles            text NOT NULL DEFAULT '{}',
    default_pickup_minutes    integer NOT NULL DEFAULT 15,
    default_delivery_minutes  integer NOT NULL DEFAULT 35,
    integration_status        text NOT NULL DEFAULT '',
    -- 00015_update_last_success: when the last Pi update finished ok (RFC 3339).
    update_last_success       text NOT NULL DEFAULT '',
    -- 00016_v220: the nearby rule for the next driver's run + Pi auto-update
    -- after Close batch.
    nearby_rule               text    NOT NULL DEFAULT 'detour',
    nearby_detour_minutes     integer NOT NULL DEFAULT 5,
    nearby_distance_mi        real    NOT NULL DEFAULT 1.0,
    auto_update               boolean NOT NULL DEFAULT true,
    -- 00017_not_delivered: simple auto-assign to the next driver (2.2.1).
    auto_assign               boolean NOT NULL DEFAULT true,
    -- 00019_callout: finishing orders + the order callout (JSON).
    finishing                 text NOT NULL DEFAULT '{}',
    callout                   text NOT NULL DEFAULT '{}',
    -- 00023_future_actions: the store's colours (JSON).
    look                      text NOT NULL DEFAULT '{}',
    -- 00025_terminal: the counter sequence's next number (0 = counter_start).
    counter_next              integer NOT NULL DEFAULT 0,
    -- 00026_terminal_driver: the last batch auto-close slot handled (UTC).
    auto_close_last           text    NOT NULL DEFAULT '',
    -- 00029_drawers_z: floor sync parking thresholds + the last parked alert.
    sync_park_refusals        integer NOT NULL DEFAULT 8,
    sync_park_hours           integer NOT NULL DEFAULT 2,
    sync_park_alerted_at      text    NOT NULL DEFAULT '',
    -- 00033_hardening: wrong PINs per terminal + per store (JSON).
    pin_throttle              text    NOT NULL DEFAULT '',
    -- 00035_terminal_app: the terminal APK this Pi has cached for its terminals (JSON).
    terminal_app_release      text    NOT NULL DEFAULT '',
    CHECK (id = 1)
);

CREATE TABLE store_config (
    id         integer PRIMARY KEY DEFAULT 1,
    context    text    NOT NULL DEFAULT '{}',
    updated_at text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (id = 1)
);

-- 00011_updates_printer_macs: learned printer hardware addresses.
CREATE TABLE printer_macs (
    printer_id text PRIMARY KEY,
    mac        text NOT NULL,
    ip         text NOT NULL,
    seen_at    text NOT NULL
);

-- 00012_pools_geo: where main located each delivery + drive times + AUTO plans.
CREATE TABLE order_geo (
    order_id     text    PRIMARY KEY,
    state        text    NOT NULL,
    lat          real,
    lng          real,
    quality      text    NOT NULL DEFAULT '',
    distance_m   integer NOT NULL DEFAULT 0,
    duration_s   integer NOT NULL DEFAULT 0,
    polyline     text    NOT NULL DEFAULT '',
    outside_area boolean NOT NULL DEFAULT false,
    store_to_s   integer,
    to_store_s   integer,
    updated_at   text    NOT NULL,
    -- 00014_integration: candidates (state check) + the confirmed address (JSON).
    candidates        text NOT NULL DEFAULT '',
    confirmed_address text NOT NULL DEFAULT '',
    -- 00018_geo_corrected: found by main's area retry + a note for staff.
    corrected         boolean NOT NULL DEFAULT false,
    note              text    NOT NULL DEFAULT ''
);

CREATE TABLE order_times (
    a       text    NOT NULL,
    b       text    NOT NULL,
    seconds integer NOT NULL,
    PRIMARY KEY (a, b)
);

CREATE TABLE auto_plans (
    id           text PRIMARY KEY,
    created_at   text NOT NULL,
    plan_json    text NOT NULL,
    confirmed_at text
);

-- 00021_settings_log: settings history (who changed what, kept 90 days).
CREATE TABLE settings_log (
    id         integer PRIMARY KEY AUTOINCREMENT,
    at         text    NOT NULL,
    section    text    NOT NULL,
    by_kind    text    NOT NULL,
    by_name    text    NOT NULL DEFAULT '',
    device_id  text    NOT NULL DEFAULT '',
    changes    text    NOT NULL
);
CREATE INDEX settings_log_at ON settings_log(at);

-- 00025_terminal
-- The snapshots main sends for the terminals (kind counter_menu |
-- terminal_config): the last good body, kept so both terminals get the same
-- one, also offline.
CREATE TABLE terminal_snapshots (
    kind       text    PRIMARY KEY CHECK (kind IN ('counter_menu', 'terminal_config')),
    version    integer NOT NULL,
    etag       text    NOT NULL,
    body       text    NOT NULL,
    fetched_at text    NOT NULL
);

-- Money taken (or given back) on a counter order at a terminal.
CREATE TABLE order_payments (
    id             text    PRIMARY KEY,
    order_id       text    NOT NULL,
    client_key     text    NOT NULL DEFAULT '',
    at             text    NOT NULL,
    tender         text    NOT NULL,
    amount_cents   integer NOT NULL,
    tip_cents      integer NOT NULL DEFAULT 0,
    tendered_cents integer NOT NULL DEFAULT 0,
    change_cents   integer NOT NULL DEFAULT 0,
    refund         boolean NOT NULL DEFAULT false,
    reason         text    NOT NULL DEFAULT '',
    ref            text    NOT NULL DEFAULT '',
    last4          text    NOT NULL DEFAULT '',
    staff_id       text    NOT NULL DEFAULT '',
    device_id      text    NOT NULL DEFAULT '',
    approved_by    text    NOT NULL DEFAULT '',
    -- 00029_drawers_z: the terminal drawer a cash payment went in ('' = none).
    drawer_id      text    NOT NULL DEFAULT '',
    -- 00031_payment_brand: a card payment's brand (amex | discover |
    -- mastercard | visa | other; '' = none given / not a card).
    brand          text    NOT NULL DEFAULT '',
    -- 00032_drawer_auto: a refund's kind ('' | goodwill | outside).
    refund_kind    text    NOT NULL DEFAULT '',
    -- 00033_hardening: the keyed request's fingerprint; the batch it was taken in.
    request_hash   text    NOT NULL DEFAULT '',
    batch_id       integer
);
CREATE INDEX idx_order_payments_order ON order_payments (order_id, at);
CREATE INDEX idx_order_payments_batch ON order_payments (batch_id);
CREATE INDEX idx_order_payments_drawer ON order_payments (drawer_id) WHERE drawer_id <> '';
CREATE UNIQUE INDEX idx_order_payments_key ON order_payments (order_id, client_key) WHERE client_key <> '';

-- Every terminal action (login, lock, order, payment, void, no sale, ...):
-- one row each, synced to main as the floor event journal.entries.
CREATE TABLE journal (
    id        text PRIMARY KEY,
    at        text NOT NULL,
    staff_id  text NOT NULL DEFAULT '',
    device_id text NOT NULL DEFAULT '',
    action    text NOT NULL,
    order_id  text NOT NULL DEFAULT '',
    detail    text NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_journal_at ON journal (at);

-- 00026_terminal_driver: a driver staff member's Pi driver.
CREATE TABLE terminal_driver_links (
    staff_id  text PRIMARY KEY,
    driver_id text NOT NULL UNIQUE,
    linked_at text NOT NULL
);

-- 00027_terminal_drafts: the terminals' shared drafts (hold + take over).
CREATE TABLE terminal_drafts (
    id                 text    PRIMARY KEY,
    location_id        text    NOT NULL DEFAULT '',
    type               text    NOT NULL,
    name               text    NOT NULL DEFAULT '',
    held               boolean NOT NULL DEFAULT true,
    holder_device_id   text    NOT NULL DEFAULT '',
    holder_device_name text    NOT NULL DEFAULT '',
    holder_staff_id    text    NOT NULL DEFAULT '',
    holder_staff_name  text    NOT NULL DEFAULT '',
    staff_id           text    NOT NULL DEFAULT '',
    staff_name         text    NOT NULL DEFAULT '',
    order_id           text    NOT NULL DEFAULT '',
    draft              text    NOT NULL DEFAULT '{}',
    created_at         text    NOT NULL,
    updated_at         text    NOT NULL
);

-- 00028_customers: the store's customers (main's feed + the terminals).
CREATE TABLE customers (
    id            text NOT NULL PRIMARY KEY,
    phone         text NOT NULL,
    phone_rev     text NOT NULL,
    first_name    text NOT NULL DEFAULT '',
    last_name     text NOT NULL DEFAULT '',
    name          text NOT NULL DEFAULT '',
    notes         text NOT NULL DEFAULT '',
    source        text NOT NULL DEFAULT '',
    updated_at    text NOT NULL DEFAULT '',
    local_at      text NOT NULL DEFAULT '',
    last_order_at text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_customers_phone ON customers (phone);
CREATE INDEX idx_customers_phone_rev ON customers (phone_rev);

CREATE TABLE customer_addresses (
    id          text    NOT NULL PRIMARY KEY,
    customer_id text    NOT NULL,
    position    integer NOT NULL DEFAULT 0,
    label       text    NOT NULL DEFAULT '',
    line1       text    NOT NULL,
    line2       text    NOT NULL DEFAULT '',
    city        text    NOT NULL DEFAULT '',
    state       text    NOT NULL DEFAULT '',
    zip         text    NOT NULL DEFAULT '',
    notes       text    NOT NULL DEFAULT '',
    is_default  boolean NOT NULL DEFAULT false,
    created_at  text    NOT NULL DEFAULT '',
    lat         real,
    lng         real
);
CREATE INDEX idx_customer_addresses_customer ON customer_addresses (customer_id);

CREATE TABLE customer_sync (
    id          integer NOT NULL PRIMARY KEY CHECK (id = 1),
    location_id text    NOT NULL DEFAULT '',
    cursor      text    NOT NULL DEFAULT '',
    synced_at   text    NOT NULL DEFAULT ''
);

-- 00029_drawers_z: cash drawer sessions (terminal + cashier shift), their
-- pay-ins / pay-outs, and the Z report per closed batch.
CREATE TABLE drawer_sessions (
    id                 text    PRIMARY KEY,
    device_id          text    NOT NULL,
    device_name        text    NOT NULL DEFAULT '',
    staff_id           text    NOT NULL,
    staff_name         text    NOT NULL DEFAULT '',
    batch_id           integer NOT NULL,
    opened_at          text    NOT NULL,
    start_cents        integer NOT NULL,
    handover_from      text    NOT NULL DEFAULT '',
    closed_at          text,
    closed_by          text    NOT NULL DEFAULT '',
    close_reason       text    NOT NULL DEFAULT '',
    counted_cents      integer,
    denominations      text    NOT NULL DEFAULT '',
    expected_cents     integer,
    flagged            boolean NOT NULL DEFAULT false,
    -- 00032_drawer_auto: opened by itself with the terminal's first payment.
    auto               boolean NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX idx_drawer_sessions_open_device ON drawer_sessions (device_id) WHERE closed_at IS NULL;
CREATE INDEX idx_drawer_sessions_batch ON drawer_sessions (batch_id);

CREATE TABLE drawer_movements (
    id           text    PRIMARY KEY,
    drawer_id    text    NOT NULL,
    kind         text    NOT NULL CHECK (kind IN ('pay_in', 'pay_out')),
    amount_cents integer NOT NULL,
    reason       text    NOT NULL DEFAULT '',
    staff_id     text    NOT NULL DEFAULT '',
    approved_by  text    NOT NULL DEFAULT '',
    client_key   text    NOT NULL DEFAULT '',
    at           text    NOT NULL
);
CREATE INDEX idx_drawer_movements_drawer ON drawer_movements (drawer_id, at);
CREATE UNIQUE INDEX idx_drawer_movements_key ON drawer_movements (drawer_id, client_key) WHERE client_key <> '';

CREATE TABLE z_reports (
    batch_id   integer PRIMARY KEY,
    id         text    NOT NULL,
    status     text    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done')),
    close_kind text    NOT NULL DEFAULT 'manual',
    forced     boolean NOT NULL DEFAULT false,
    report     text    NOT NULL DEFAULT '',
    created_at text    NOT NULL,
    built_at   text,
    -- 00033_hardening: doesn't balance (paged once).
    flagged    boolean NOT NULL DEFAULT false
);

-- 00033_hardening: CREATE INDEX idx_orders_carried ON orders (carried) WHERE carried;
-- and the S-6 triggers (order_payments_money_ins / _order_ins / _money_upd,
-- drawer_sessions_money_ins, drawer_movements_ins, batches_one_open) live in
-- the migration only (sqlc doesn't need them; the drift test compares tables
-- and indexes).
CREATE INDEX idx_orders_carried ON orders (carried) WHERE carried;

-- 00034_journal_key: a terminal's journal rows deduplicated by detail.client_key.
CREATE INDEX idx_journal_client_key ON journal (device_id, action, json_extract(detail, '$.client_key'))
WHERE json_extract(detail, '$.client_key') IS NOT NULL;
