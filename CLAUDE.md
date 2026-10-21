# CLAUDE.md

This file is loaded into Claude Code's context when working in `agent/`. For
the external contract see `contract/agent.md` at the repo root.

## Project overview

The agent is the restaurant-side printer bridge. Long-lived SSE connection
to `main/` at `/api/v1/events/orders`, authenticated by an
`X-STORE-API-KEY` whose scope identifies which (tenant, location) this
agent belongs to. On every connect it receives a `store_config` frame
(tenant + location + printers) and persists it locally. Each subsequent
`event: order` frame is a canonical `Order` Snapshot which the agent
persists into the open **batch**. The agent **owns the floor flow** — accept
(kitchen number + prep estimate) → ready → driver run → delivered, drivers,
batches — prints per-station tickets + the customer receipt on accept, and
syncs every floor change to main (see "Floor flow" below).

The agent talks canonical Snapshot from end to end. There is no
anti-corruption layer over main's events — main is the source of truth and
the same shape flows through agent → SQLite → mobile UI.

## Commands

The agent is a **single static CGO-free binary** now — no Docker, no Postgres,
no nginx. It stores everything in one **pure-Go SQLite** file and serves its own
web UI. Dev, staging, and prod all run it the same way (`doppler run -- ./agent`).

```bash
task build                 # → ./bin/agent (host arch)
task build-arm64           # → ./bin/agent-linux-arm64 (static, CGO_ENABLED=0) — what the Pi runs
task dev                   # hot-reload dev run via Air (AGENT_DB_PATH=sql/agent.db). The console uses this.
task run                   # doppler run -- go run ./cmd/main.go
task run-no-doppler        # when env is already exported
task test                  # pytest under Doppler (dev main must be up; spawns its own throwaway agents)
task test-quick            # same, minus `slow` tests (the ~30s revoke-detection wait)
task test-real-printer     # opt-in, TEST_REAL_PRINTERS=1; burns paper
task test-go               # Go unit tests (SQLite order + settings repo round-trips, OTEL resource) — no secrets
task sqlc                  # regenerate sqlc models (keep sql/schema.sql mirroring ALL migrations)
task release -- patch      # version bump + push v* tag → CI cuts an immutable arm64 release (see RELEASING.md)
```

**Dev roster passwords:** the console's `wire` sets web-UI password `aaryan`
only on agents it provisions; an agent whose password was set by hand needs
`AGENT_UI_PASSWORD` for the console smoke / E2E lifecycle test (the floor routes
need a session or a tablet key now).

**Dev DB**: the console (and root `task dev`) runs a **roster** — one agent per
seeded restaurant, each with `AGENT_DB_PATH=sql/agent-<slug>.db` and its own port
(Lombardi's :8080, Golden Dragon :8081, Taco Fiesta :8082); this module's
`task dev` runs a single agent on `sql/agent.db`. All gitignored, and they
**persist across restarts** — the provisioned token + local password survive;
re-provisioning is only needed after the root `task dev-reset` (wipes
`sql/agent*.db*`) or a manual DB delete. The console's `wire all` provisions
every running roster agent (mint token → paste → set password `aaryan`). Each
roster agent's `air` builds into its own `tmp/<slug>/agent`.
Requires
`air` on PATH (`go install github.com/air-verse/air@latest`); without it the
console falls back to `go run`.

**Deploy (staging + prod Pi, aarch64):** CI publishes a static `linux/arm64`
binary — a rolling `staging` prerelease on every branch push, and an immutable
release per `v*` tag. Download it to the Pi and run it:

```bash
gh release download staging-latest -p 'agent-linux-arm64*'   # or a v1.2.3 tag
sha256sum -c agent-linux-arm64.sha256
doppler run -- ./agent-linux-arm64                    # stop with: pkill agent
```

As-built staging differs from the above: the Pi `192.168.1.219` was planted
manually and runs from an env-file snapshot rendered from Doppler (not live
`doppler run` — DietPi Doppler CLI wouldn't install). See `../STAGING.md`.

- The pytest suite **builds the binary once and spawns throwaway agents** (temp
  SQLite, spare port, `MAIN_SERVER_URL`=dev main, no OTEL) plus a throwaway
  tenant/location in main per test — so it drives the provisioning state machine
  end-to-end without touching the console's roster agents. It needs main's
  `ADMIN_API_KEY`: from the env, else read with `../main/.doppler-dev-main`
  (inherited `DOPPLER_*` vars are stripped for that nested lookup — an outer
  `doppler run` exports `DOPPLER_PROJECT=agent`, which would hijack the token's
  scope). Only the real-printer tests target an already-running agent
  (`AGENT_BASE_URL`, default `:8080`).
- Agent uses Doppler for env (dev/stg/prd configs — user-managed). No Postgres:
  the DB is a local SQLite file at `AGENT_DB_PATH` (default `~/.quicc/agent.db`).

## Architecture (what's non-obvious)

**Entry-point wiring order in `cmd/main.go`** (order matters):

1. `config.Load()` validates `MAIN_SERVER_URL` (only required var). There is
   no `STORE_API_KEY` env var — a blank agent boots `unconfigured` and waits
   for an admin-minted token to be pasted into its setup screen.
2. `observability.Setup()` **before** the logger is wrapped — empty
   `OTEL_EXPORTER_OTLP_ENDPOINT` installs no-op providers so the agent
   runs fully offline.
3. Logger is then `observability.Wire`'d so every `logger.X().Ctx(ctx)...`
   line gets `trace_id`/`span_id` fields AND goes out via the OTLP log
   bridge.
4. SQLite file opens (`AGENT_DB_PATH`) + goose migrations run. Order + settings
   + storeconfig repos are built.
5. **Hydrate**: read persisted `store_config` from DB. If present, hot-load
   the printer roster into the Dispatcher so the agent keeps printing
   across restarts even if main/ is unreachable.
6. SSE Controller is constructed and given the device token read from the
   DB (`settings.store_api_key`, the only source — no env fallback). Empty key
   → no SSE connection until the setup screen provides one. The Controller's `SetKey` is the only API surface — it
   cancels the current SSE goroutine and starts a fresh one with the new key.
7. HTTP server + SSE controller + retention sweeper start as goroutines;
   `<-ctx.Done()` blocks until SIGINT/SIGTERM.

**Clean Architecture layout inside `internal/`:**

- `domain/order/` — canonical Snapshot (mirrors main/'s shape), Status,
  StoredOrder, repository interfaces.
- `domain/storeconfig/` — agent-side mirror of main's `StoreContext`.
- `domain/printer/` — Printer interface, PrintJob value type.
- `application/order/` — order service: OnArrival, Accept, Reprint. Owns
  the Pushover sound decision (cash/credit/big-ticket).
- `application/printer/` — Dispatcher (dynamic roster from store_config),
  Service (per-printer KeepCheck + Print), Registry (in-memory reachability
  cache for the mobile API).
- `application/sseclient/` — Controller. Wraps the raw `infra/sse/client`
  with `SetKey` semantics so the web UI can swap keys at runtime.
- `application/retention/` — count-based rolling-window trimmer (keep newest N).
- `infra/database/` — SQLite (modernc) handle, sqlc models, repos, manual DB spans.
- `infra/mainclient/` — HTTP client to main/ (token verify + history/config proxy + self-revoke).
- `infra/sse/` — SSE client (handshake + order parse, trace propagation).
- `infra/ssebroker/` — mobile-facing SSE fan-out.
- `infra/printer/escpos/` — TCP→port 9100 ESC/POS adapter.
- `infra/printer/receipt/` — receipt builder against canonical Snapshot +
  StoreContext.
- `infra/notify/` — Pushover.
- `transport/` — HTTP server exposing the mobile-facing routes.

## SSE client (non-obvious behaviours)

`internal/infra/sse/client.go`:

- HTTP/1.1 is **forced** by nil-ing `TLSNextProto` on the Transport. HTTP/2
  multiplexed streams get terminated by upstream proxies on idle timeout —
  do not revert.
- Exponential backoff 2s → 60s on disconnect.
- Three event types handled:
  - `event: store_config` — handshake frame. Unmarshalled to
    `storeconfig.StoreContext`; persisted via `storeConfigSink.Apply`,
    which hot-swaps the printer dispatcher's roster and the receipt
    builder's atomic context.
  - `event: order` — canonical Snapshot. If `_traceparent` is present, the
    span context is extracted so the agent-side `sse.receive` span chains
    under main's `sse.publish` span.
  - `event: command` — a duplex RPC envelope; routed to the `rpc.Registry`
    (see below) rather than the order path.
- Scanner buffer is 1 MB/line — default 64 KB is too small for orders with
  many items.

## Duplex RPC channel (`internal/rpc/`)

Reusable request/reply over the two existing transports — **no per-feature
endpoints**, NAT-safe (the agent always dials main). Envelope `{type, id,
payload}` mirrors `main/.../internal/rpc`; wire shape is pinned in
`contract/agent.md` §2.4b.

- main→agent commands arrive as `event: command` SSE frames → `Registry.Handle`
  decodes, runs the handler, and POSTs the reply (reusing the request `id`) to
  `POST /store/v1/agent/rpc` on main via `mainclient.PostRPC` (store-key auth).
- Unsolicited **reports** use `Registry.Report` (fresh `id`, no waiter).
- Handlers are registered in `cmd/main.go`. The registry is threaded through
  `sseclient.Controller` → `sseraw.New`; its `ReplyPoster.key` getter is wired
  to `sseCtrl.CurrentKey` **after** controller construction (breaks the
  registry↔controller construction cycle).
- **`printer.report`** (`application/printer/report.go`): `BuildReport` reuses
  `Registry.All()`/`StatusSnapshot` — pushed on every `store_config` apply
  (`storeConfigApplier.postApply` hook) + a ~30 s ticker, and pulled on demand
  via the `printer.report.request` handler. Feeds main's advisory
  `GET /store/v1/me/printers/live`.

## Provisioning: token + local password (state machine)

A built agent is **generic** — not tied to any location until a token is pasted
in. `GET /api/session` returns a `state` that drives the whole UI:

| state | condition | screen |
|---|---|---|
| `unconfigured` | no token | paste an admin-minted token |
| `needs_password` | token linked, no password | create a local password |
| `locked` | token+password, no valid session | unlock with the password |
| `ready` | token+password+valid session | the app |

**Token** (`settings.store_api_key`, the ONLY source — no env var). Provisioned by
`POST /api/setup/token {token}`: the handler **verifies the token synchronously**
via a device-authed `GET /store/v1/store-config` against main (200 valid / 401
`invalid_token`) — deliberately NOT through the SSE controller, so there's no
race with the connection state. On success it persists the token + `SetKey`s it.
`SetKey` is the only mutation surface (cancels the SSE goroutine, restarts).

**Local password** (`settings.ui_password_hash` bcrypt + `ui_password_hint`). Set
once via `POST /api/setup/password {password, confirm, hint?}` (no minimum length,
must match), verified on `POST /api/login {password}`, rotated in Settings
(`POST /api/settings/password`). No recovery — a forgotten password means
re-provision. `GET /api/hint` feeds the login screen's "Hint?" link.

**Unlink** (Settings → Unlink): `POST /api/settings/unlink` → `mainclient.RevokeSelf`
→ main `POST /store/v1/agent/revoke` → `ClearProvisioning` (wipes token **and**
password) + `SetKey("")` + clears the session → `unconfigured`. (Vs. **Log out**,
which only ends the web session → `locked`, token+password intact.)

**Auto-wipe on revoke** (`cmd/main.go` `wipeProvisioning`, wired to BOTH
`sseCtrl.OnRejected` and `mainClient.SetOnUnauthorized`): when main **definitively
rejects** the token (a **401** — device revoked/hard-deleted), the agent
`ClearProvisioning`s + `SetKey("")`s → the UI drops to the setup screen. It fires
from **any** agent→main 401, not just an SSE reconnect: a dashboard revoke leaves
the live SSE stream open, so the reset is driven by the **30s printer-report
heartbeat** (or any history/store-config proxy call, or an RPC reply) getting 401
via `mainclient.do()`'s centralized hook — idle detection ≤30s, instant on any
user action. `wipeProvisioning` is idempotent + **key-checked** (it only wipes if
the rejected key still matches the stored one), so a second 401 no-ops and a stale
401 for a since-rotated token can't nuke a freshly-pasted one. A plain
unreachable-main is a connection error (not a 401), so a network blip never wipes.

**Web session** (`session.go`): the login cookie is signed with a **persisted**
secret (`settings.session_secret`, resolved in `cmd/main.go` via
`resolveSessionSecret`), so sessions **survive agent restarts**. It's a
browser-session cookie (dropped on close) with a 24h cap in the signed payload.

**Connection status (green light)**: `internal/infra/sse/client.go` holds a
shared `ConnState` (atomics: connected / authRejected / connectedSince /
lastEventAt / reconnects / lastError + an `onRejected` callback) that the
`Controller` owns across key swaps and exposes via `Status()`. `connect()` sets
`connected` on 200, `markRejected()` latches `authRejected` (+ fires the wipe) on
**401**, and `dispatch()` stamps `lastEventAt` on every frame. `GET /api/session`
surfaces `sse_state`; the web UI header polls it (~5s) → green / amber / red.

## Printer roster from store_config

There are no `PRINTER_IP` env vars anymore. The Dispatcher receives the
printer list on every SSE handshake from main and rebuilds the roster
whole-cloth: cancels prior KeepCheck goroutines, instantiates fresh
Service+Printer pairs, starts new KeepChecks.

## Deploying on a Pi

`deploy/quicc-agent.service` (systemd, `doppler run`, restart always, `:8080`)
+ `deploy/README.md`. The restaurant Pi still runs **v1** (Docker + Postgres +
nginx :80) — the switch is in `../CUTOVER.md`; never run both agents at once.

## Floor flow (agent decides, main mirrors)

- **State lives here**, one writer: `orders.state` (`arrived|accepted|ready|
  on_delivery|delivered|cancelled` — Close batch cancels never-accepted orders) + floor columns, `batches` (exactly one open),
  `drivers` (queue = `position`), `settings.kitchen_next` (1100, reset on Close
  batch) / `last_prep_minutes`. Transitions are in
  `infra/database/repositories/floor_repo.go`; **each one is a single tx that
  also inserts its `floor_events` row(s)** — keep it that way (the outbox is
  what makes main eventually consistent). The `accepted` event is written only
  after something printed (`RecordAccepted`).
- **Sync**: `application/floorsync` POSTs the outbox to main as one
  `floor.events` RPC envelope (≤50, oldest by **rowid**), marks every event main
  lists as synced, backs off 5 s → 5 min; a 202 (old main) waits 5 min. Woken by
  `orderService.OnFloorEvent`.
- **Auth**: floor routes (`Auth: "device"` in `routes.go`) accept a paired
  tablet's `X-Agent-Device-Key` **or** the web-UI session (`transport/pairing.go`
  `floor()`); pairing codes are in memory (6 digits, 5 min, single use).
- **Drivers**: `off | available | out`; clock-in (back of the queue, opens a
  `driver_shifts` row) / clock-out (only when `available`); only `available`
  drivers can be assigned (`ErrDriverNotAvailable` → 409). Daily auto
  clock-out at `settings.driver_reset_time` (location tz, default 10:00) via
  `application/driverreset`; drivers out on a run are clocked out when they
  return. Assign publishes SSE `drivers.dispatched` (name flash). Shifts sync
  to main as `driver.shift` events (report record, not wages).
- **Escalation**: `application/escalation` (15 s tick) → `EscalateOverdue`
  (2 min, repeating, `last_alert_at` makes it restart-safe).
- **Retention** is by batch (`AGENT_KEEP_BATCHES`, default 3 closed + the open
  one); `AGENT_ORDER_RETENTION` is gone.
- Timestamps written from Go use `fmtTS` (same text shape as SQLite's
  `strftime('%Y-%m-%dT%H:%M:%fZ')`) so SQL text comparisons stay correct.

## Hardening (final review pass, 2026-10-16)

- **Catch-up after every SSE reconnect** (`application/catchup`, wired via
  `sseCtrl.OnConnected`): main marks a frame delivered once it's queued, so
  frames buffered when the stream drops were lost. After connect (+3 s) the agent
  pulls main's `status=received` orders since the open batch opened (≤12 h) and
  runs `OnArrival` for each — which now **ignores orders it already holds**
  (`UpsertArrived` reports `inserted`), so redeliveries never re-announce or
  re-accept an order.
- **Accept**: the `accepted` floor event is written with
  `context.WithoutCancel` (a tablet timeout can't lose it); printing is capped
  at 25 s per order, dials honour the context, fallbacks the health check saw
  down are tried last, print-trouble Pushovers are sent in the background.
- **Mobile SSE** buffer is 128 and a client that can't keep up is
  **disconnected** (it reconnects + refetches) instead of silently missing
  events.
- **Web sessions**: the signing secret rotates (and is re-persisted) on a
  password change, unlink, or revoke-wipe → every other browser is signed out;
  unlink/wipe also **revoke all paired tablets**. `/api/login` is throttled per
  IP (5 misses → 30 s, doubling, max 15 min → 429 `too_many_attempts`).
- Drivers: can't remove one who is `out` (409); removing a clocked-in driver
  closes the shift; "returned" only for `out` drivers; the daily reset's first
  check after install/upgrade only starts the clock, and changing the time never
  fires a reset immediately; close batch clocks out drivers whose shift began
  before the last reset.
- `cmd/main.go` creates the DB directory (`~/.quicc` on a fresh Pi); ticket
  names are cut by characters, not bytes; floor sync isolates an event main
  rejects with 400/413 instead of retrying the batch forever.

## Driver reports, undo-return, settings, app cache (2026-10-16)

- **Driver reports** (`application/order/reports.go`, repo `DriverReports`,
  `receipt/report.go`): one slip per driver who delivered in, or was clocked in
  during, a batch. Cash = the order's *cash* payments (card = paid to the store);
  tips A/T = all tips × (1 − `tip_tax_bps`); owed = cash − tips A/T (negative →
  "STORE OWES DRIVER"). Printed by `Dispatcher.PrintDocument` (receipt printer,
  failover) **in the background after Close batch** and on demand via
  `POST /api/batches/{id}/driver-reports/print`. Golden: `testdata/driver_report.golden`.
- **Undo return**: `DriverReturned` remembers the driver + old queue position
  (`settings.last_return_*`); `UndoReturn` (latest only, ≤10 min, still
  available) sets them `out` again. Orders stay delivered (main is forward-only).
- **Floor prefs** (`settings.green_till_minutes`, `tip_tax_bps`, migration
  `00007_floor_settings`): web-UI `PUT /api/settings/floor`, exposed in
  `/api/floor`, SSE `floor.updated`.
- **Quibase cache** (`application/appupdate`): polls main's
  `/store/v1/agent/app/latest` on SSE connect + every 5 min, downloads newer APKs
  (sha256-checked) to `<db dir>/apk/`, serves `/api/app/latest` + `/api/app/apk`.
  Version codes are Unix seconds.

## Headless recovery + delivery guarantee (2026-10-16)

- **Manager-login link** (`application/recovery`, `POST /api/setup/link`): the
  normal provisioning path now. main mints the key, revokes older agents and
  hands secrets + the manager bcrypt hash; the open batch is restored
  (`RecoveryRepository.Restore`, one tx, blank DB only) **before** the key is
  stored. The Tailscale key goes to `/var/lib/quicc-agent/tailscale.key`, never
  the DB. Linked agents unlock the web UI with the manager login
  (`settings.manager_username`; hash refreshed from main on connect and once on
  a failed login); local password change answers 409 `manager_login`.
- **Secrets in SQLite** (`settings.secret_*`) are applied at startup **before**
  `observability.Setup` (that's why the DB now opens first) — env always wins.
- **Acks** (`application/acker`): `orderService.OnStored` fires for new AND
  duplicate arrivals; main re-sends until acked. **Reconciliation**:
  `catchup.Reconcile` every 60 s while connected.
- **State + health** (`application/statesync`, `application/health`): RPC reports
  `agent.state` / `agent.health`; `OnFloorEvent` wakes both floor sync and
  state sync. **Nightly backup** (`application/backup`): `VACUUM INTO` → main.
- **`accept_pending`** (migration `00009`): set by ClaimAccept, cleared with the
  accepted floor event in one tx (`RecordAccepted`); `RecoverInterruptedAccepts`
  at startup reverts leftovers to `arrived` (they ring again).
- Tests: `repositories/recovery_repo_test.go` (swap at repo level),
  `tests/test_link.py`; the full mid-shift swap is the root `tests/test_swap.py`
  and the delivery torture is `task test-torture` (root).

## Quibase setup + diagnostics (2.0.1)

- **LAN discovery** (`infra/discovery/responder.go`, `AGENT_DISCOVERY_PORT`,
  default 48080, `0` off): UDP probe `{"q":"quibase-discover","v":1,"nonce"}`
  padded to ≥ 200 bytes → unicast reply `{v, svc, nonce, port, linked,
  link_available, store, version, host}`, always smaller than the probe (store/
  host dropped if needed), 20 replies/s. Bind failure only warns — the dev
  roster's 2nd/3rd agents can't get 48080; pytest spawns agents with
  `AGENT_DISCOVERY_PORT=0` unless a test passes a free port.
- **Device routes** in `transport/server_diag.go` (wired by `SetDiag` in
  `cmd/main.go`): `GET /api/diag` (main link + live `/health` reachability
  cached 20 s, `health.Snapshot` incl. parsed throttled bits/staleness, roster,
  orders, `open_now` from `storeconfig/hours.go` — a port of main's
  `tenancy/hours.go`, keep in step — `quiet_alert_minutes` location config,
  default 90, `problems`), `POST /api/printers/test` (`Dispatcher.PrintTest` /
  `Probe` — **never fails over**; `testMu` → 409), `PATCH /api/device/self`
  (sqlc `RenamePairedDevice`), `POST /api/device/report-token` (proxies main's
  `/store/v1/agent/tablet-tokens`; never stored), `POST /api/diag/report`
  (≤ 256 KB → RPC `diag.report` via `Registry.Send`, which returns main's reply;
  tablet id/name from the device key only). `POST /api/pairing/code` now takes a
  device key too. Unpair/revoke fire a background `DELETE
  /store/v1/agent/tablet-tokens/<device id>`.
- `GET /api/session` adds `store_name`, `version`, `hostname`.
- `internal/infra/database/ascii_test.go` fails the Go suite if `sql/query.sql`
  gains a non-ASCII byte.

## Printing (station tickets + receipt)

main stamps `printer_id` + `routing` on each line item at ingest; the agent
splits. `Dispatcher.Execute` → `Plan` (pure, `application/printer/plan.go`) →
one station ticket per printer with items (own items full, rest small) + the
customer receipt on the `is_receipt` printer; unrouted items go to every
station; orders with no `routing` at all print one full receipt (legacy). Each
ticket fails over (receipt printer → others) with a REROUTED banner; Execute
errors only if nothing printed. Roster is keyed by printer **id** (rename-safe).

- **Accept = claim, then print** (`ClaimAccept` conditional UPDATE → print
  outside any tx → `RevertAccept` only if nothing printed). Don't move printing
  back inside a transaction: the pool is one SQLite connection and a dead
  printer's dial timeout would stall every other query.
- One Pushover per print for failed/rerouted tickets (+ unrouted items on accept).
- `escpos` dials `ip` as-is when it's `host:port` (fake printers in pytest),
  else `ip:9100`.
- Receipt bytes: `receipt.Build(snap, sc, ticket)`; station layout pinned by
  `receipt/testdata/station.golden` (`go test ./internal/infra/printer/receipt -update`).
- **Keep `sql/query.sql` pure ASCII** — a non-ASCII char (e.g. `→`) in a comment
  makes sqlc's sqlite engine mis-slice every later query (runtime "syntax error").

## Trace propagation

- Inbound HTTP: `otelhttp.NewHandler` on the mux extracts inbound
  `traceparent` headers.
- Every HTTP response echoes its trace id as `X-Trace-Id` (a
  `traceIDHeader` wrapper *inside* otelhttp so the span exists already) —
  parity with online/ and main/'s Echo middleware. CORS-exposed.
- Inbound SSE: extracted from `_traceparent` in the event body.
- Outbound HTTP (Pushover, SSE dial): `otelhttp.NewTransport` wraps the
  clients.

## Pushover

Problems only, two audiences (`infra/notify`): `Send` → the owner
(`PUSHOVER_USER_KEY`) for system events (agent started); `SendFloor` → the
owner **and** the boss (`PUSHOVER_BOSS_KEY`, optional) for floor problems —
unaccepted 2 min+ (`siren`), nothing printed / rerouted / unmatched items
(`siren`), printer down (`printer-error`) / back up (`classical`). There is no
per-accept notification anymore (the old cash/credit/obama sounds were
removed — they buried the real alerts).

## Config surface

Required: `MAIN_SERVER_URL`.
Optional: `AGENT_DB_PATH` (SQLite file, default `~/.quicc/agent.db`),
`AGENT_KEEP_BATCHES` (closed batches kept locally, default 3),
`HTTP_PORT` (8080), `HTTP_ALT_PORT` (second port with the same handler — 80 on a Pi; off by default), `PRINTER_DETECT_DELAY` (25 s), `LOG_LEVEL`,
`LOG_OUTPUT` (`console`|`json`|`file`; `file` → lumberjack-rotated
`log/agent.log`), `PUSHOVER_APP_TOKEN`, `PUSHOVER_USER_KEY`, `PUSHOVER_BOSS_KEY`,
`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME` (default `quicc-agent`),
`AGENT_DISCOVERY_PORT` (UDP LAN discovery, default 48080, `0` off).

`OTEL_EXPORTER_OTLP_ENDPOINT` accepts a full URL or a bare `host:port`
(defaults to `http://`). OTLP transport is **HTTP** here, not gRPC; use
`:4318` for the Collector. Export failures go through a rate-limited OTEL error
handler (first error, then ≤1 line/min with a suppressed count, to plain stderr
— never through the OTLP log hook, so it can't feed itself); exports time out
at 5s. The OTLP log hook itself is in-repo (`observability/zlog.go`).

**Removed env vars** (legacy, no longer read): `DATABASE_URL` (Postgres is gone —
SQLite via `AGENT_DB_PATH`), `AGENT_API_KEY` / `STORE_API_KEY` (the token now
lives only in SQLite, pasted via the setup screen),
`PRINTER_IP` / `PIZZA_PRINTER_IP` / `DESI_PRINTER_IP` / `SUB_PRINTER_IP` /
`WINGS_PRINTER_IP` (printers come from store_config).

## Persistence — pure-Go SQLite

`internal/infra/database` opens one `modernc.org/sqlite` file (CGO-free, so the
binary stays static) with WAL + `busy_timeout` + `foreign_keys` and a single open
connection. `internal/migrate` runs goose (`sqlite3` dialect) on boot
(`00001_initial_schema`, `00002_session_secret`, `00003_ui_password`,
`00004_floor` + `00005_cancelled` — each rebuilds `orders` to widen the state CHECK;
`00006_driver_shifts` rebuilds `drivers` (adds `off`) + `driver_shifts`; use
`migrate.RunTo` in tests to seed an older schema). sqlc
targets the `sqlite` engine (`sql/sqlc.yaml`, `sql_package: database/sql`);
`sql/schema.sql` mirrors **all** migrations and every query — including the
settings/provisioning ones — goes through sqlc (`task sqlc`).

**modernc pairing gotcha:** `modernc.org/sqlite` requires the *exact*
`modernc.org/libc` version in its own go.mod. A blanket `go get -u` bumps libc
past it — re-pin with `go get modernc.org/libc@<version sqlite asks for>` (check
`$(go env GOMODCACHE)/cache/download/modernc.org/sqlite/@v/<ver>.mod`).
There's no `otelpgx` anymore — each repo method wraps its call in a manual span
(`repositories/span.go`) so DB timings still land in Grafana. Timestamps are
stored as RFC3339 text and parsed back in the repos.

**Batch window**: the agent keeps the open batch + the newest
`AGENT_KEEP_BATCHES` (default 3) closed batches; `application/retention` sweeps
hourly (and drops synced floor events older than 7 days). main/ holds the full
history — the web UI's History tab reads it via proxy.

## Web UI (served by the binary)

`internal/transport/web/index.html` is embedded (`go:embed`) and served at `/` by
the agent itself — no nginx. It's a password-gated SPA whose three auth panels
(setup / create-password / unlock) are driven by `GET /api/session`'s `state`:

- **Provisioning**: paste the admin-minted token (`POST /api/setup/token`) →
  create a local password (`POST /api/setup/password`) → the app. Thereafter
  `POST /api/login {password}` unlocks; `POST /api/logout` ends the session. A
  signed session cookie (persisted secret, `session.go`) gates the web-UI-only
  routes. See "Provisioning: token + local password" above for the full lifecycle.
- **Live tab** = the **backup floor screen**: waiting (Accept with a prep pick)
  / in kitchen (Ready) / ready deliveries (tick + Send out with a driver) / out
  with drivers (Returned) / done, batch + next kitchen #, Close batch (confirms
  leftovers). Live via its own `/api/events` (session cookie). **Drivers** tab:
  add/rename/remove/reorder. **Settings**: Pair tablet (6-digit code) + paired
  tablets (revoke). Cards also have **Reprint**
  (`POST /api/orders/{id}/reprint`, print only) and **Resend**
  (`POST /api/orders/{id}/resend`): an order still `arrived` is re-announced to
  mobile (and printed if auto-accept is on); an `accepted` one is reprinted. (The
  arrival upsert is insert-only, so Resend can't just re-run `OnArrival`.)
- **History tab**: `GET /api/history` proxies main's `GET /api/v1/orders`
  (paginated + search/filter/sort) using the stored key. Reprint of an old order
  POSTs its fetched payload to `POST /api/print` (print only); Resend →
  `POST /api/resend` (full arrival pipeline if the agent never had it, else as
  above).
- **Store-config tab** (read-only): `GET /api/store-config` proxies main's
  `GET /store/v1/store-config`. Store details + printers are edited only in
  main's `/app/store`, which pushes a fresh `store_config` frame over SSE; the SSE
  client applies mid-stream frames exactly like the handshake (roster rebuild,
  persist, printer report). The old POST/write-through + forced reconnect is gone.

**Auth split**: floor routes (`/api/floor`, `/api/orders*`, `/api/deliveries/*`,
`/api/drivers*`, `/api/batches`, `/api/batch/close`, `/api/printers*`,
`/api/events`) and the Quibase setup/diagnostics routes (`/api/pairing/code`,
`/api/device/self`, `/api/device/report-token`, `/api/diag`, `/api/diag/report`,
`/api/printers/test`) need a paired tablet key **or** the web-UI session — no
longer LAN-open; `POST /api/pair` is open (needs a live code); the web-UI surface
(history, print/resend, store-config, settings, devices) is session-gated.
`internal/infra/mainclient` is the agent's HTTP client to main.

## Files to look at for…

| To learn about | Look at |
|---|---|
| SSE client (handshake + order parse) | `internal/infra/sse/client.go` |
| SSE controller (key swap) | `internal/application/sseclient/controller.go` |
| Order domain (canonical Snapshot mirror) | `internal/domain/order/order.go` |
| store_config domain | `internal/domain/storeconfig/entity.go` |
| Printer dispatcher (dynamic roster, failover) | `internal/application/printer/dispatcher.go` |
| Test print / probe (no failover) + test slip | `internal/application/printer/testprint.go`, `internal/infra/printer/receipt/test_slip.go` (golden `testdata/test_slip.golden`) |
| Diagnostics, self-rename, report token/relay | `internal/transport/server_diag.go` |
| LAN discovery (UDP responder) | `internal/infra/discovery/responder.go` |
| Opening hours (port of main's) | `internal/domain/storeconfig/hours.go` |
| Pi health (heartbeat, `Snapshot`, throttled bits) | `internal/application/health/{health.go,system.go}` |
| Ticket planning (station split) | `internal/application/printer/plan.go` + `internal/domain/printer/ticket.go` |
| Floor transitions (tx + outbox) | `internal/infra/database/repositories/floor_repo.go` |
| Floor service / escalation / sync | `internal/application/order/floor.go`, `application/escalation/`, `application/floorsync/` |
| Floor routes + tablet pairing | `internal/transport/server_floor.go`, `internal/transport/pairing.go` |
| Duplex RPC (envelope + registry) | `internal/rpc/{envelope.go,registry.go}` |
| Printer-roster report to main | `internal/application/printer/report.go` (wired in `cmd/main.go`) |
| Receipt builder | `internal/infra/printer/receipt/builder.go` |
| HTTP route table (single source of truth) | `internal/transport/routes.go` — `routeDef` slice drives the mux **and** `GET /__routes` |
| HTTP server wiring | `internal/transport/server.go` (`Start` iterates the table) |
| Schema | `sql/schema.sql` + `internal/migrate/sql/*.sql` |
| Env vars | `internal/config/config.go` |
| Web UI (login, tabs, reprint/resend) | `internal/transport/web/index.html` |
| Web-UI handlers + session gate | `internal/transport/server_web.go`, `session.go` |
| HTTP client to main/ (verify + proxy + self-revoke) | `internal/infra/mainclient/client.go` |
| CI (arm64 releases) | `.github/workflows/build.yml` |
