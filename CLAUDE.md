# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

The parent `/projects/quicc/CLAUDE.md` already describes the full system topology, the `online → main → agent → printer` order flow, the observability conventions (traceparent propagation, zerolog + OTLP bridge, parent-based always-on sampling), and the deployment layout (prod = restaurant w/ 5 printers, staging = home-network desktop w/ 1 printer). **Read that first.** This file covers only what's specific to the `agent/` sub-project.

## Commands

```bash
task build                 # → ./bin/agent
task run                   # doppler run -- go run ./cmd/main.go
task run-no-doppler        # when env is already exported (e.g. systemd)
task test                  # pytest under Doppler; agent MUST already be running
task test-real-printer     # opt-in, TEST_REAL_PRINTERS=1; burns paper
task test-go               # reserved; no Go tests yet
task devup / devupd / devdown   # docker compose (agent + nginx web UI)
```

- Run a single pytest: `doppler run -- uv run --directory tests pytest -v tests/test_http_api.py::test_get_orders_returns_list`.
- Tests do NOT boot the agent; they hit `AGENT_BASE_URL` (defaults to `http://localhost:${HTTP_PORT:-8080}`) and `pytest.skip` the suite if it isn't reachable within 5 s. Start the agent first.
- The `real_printer` and `sse` pytest markers are registered in `tests/pyproject.toml`. `real_printer`-marked tests are auto-skipped unless `TEST_REAL_PRINTERS=1`.
- Agent uses Doppler for env (dev/stg/prd configs — user-managed). The tracked `.env` file is legacy and not read by `task run`.

## Architecture (what's non-obvious)

**Entry point wiring order in `cmd/main.go`** (order matters):
1. `config.Load()` validates `MAIN_SERVER_URL`, `AGENT_API_KEY`, `PRINTER_IP` — fails fast if missing.
2. `observability.Setup()` **before** the logger is wrapped — empty `OTEL_EXPORTER_OTLP_ENDPOINT` installs no-op providers so the agent runs fully offline.
3. Logger is then `observability.Wire`'d so every `logger.X().Ctx(ctx)...` line gets `trace_id`/`span_id` fields AND goes out via the OTLP log bridge.
4. Five `escpos.New` + `printerApp.NewService` pairs are created (Pizza, Desi, Sub, Wings, Online). In staging only `PRINTER_IP` (the Online printer) is reachable; the others will log unreachable on the KeepCheck loop and that's expected.
5. **Only `onlineService` is wired into `orderApp.NewService`.** Per-category routing to Pizza/Desi/Sub/Wings is not implemented — every order currently prints to the Online printer. If adding routing, do it in `internal/application/order/service.go:Handle`.
6. SSE client + HTTP server start as goroutines; `<-ctx.Done()` blocks until SIGINT/SIGTERM.

**Clean Architecture layout inside `internal/`:**
- `domain/` — pure types (`order.OrderRequest`, `printer.Printer` interface, `printer.PrintJob`).
- `application/` — use-cases (`order.Service.Handle`, `printer.Service.Print`/`KeepCheck`). Owns tracers, meters, and the business decision of which Pushover sound to play.
- `infrastructure/` — adapters: `escpos/` (TCP→port 9100), `sse/` (main server listener), `mainclient/` (HTTP GET against main for backlog), `notify/` (Pushover).
- `transport/` — HTTP server exposing `/api/orders` (list) and `/api/orders/{id}/reprint`.
- `store/` — in-memory, 24-hour-TTL map keyed by order ID. Cleanup runs every 10 min. **Not persisted** — agent restart wipes the store.
- `observability/` — `otel.go` (providers + shutdown), `zlog.go` (trace-id hook + OTLP log bridge), `meters.go` (custom instruments: `orders.printed`, `printer.status`, `printer.detect.duration_ms`, `printer.write.duration_ms`, `sse.reconnects`).

**SSE client — several non-obvious behaviours** (`internal/infrastructure/sse/client.go`):
- HTTP/1.1 is **forced** by nil-ing `TLSNextProto` on the Transport. HTTP/2 multiplexed streams get terminated by upstream proxies on idle timeout, which was causing spurious reconnects — do not revert this.
- Exponential backoff from 2 s to 60 s on any disconnect; reconnect counter published via `sse.reconnects` counter.
- **Backlog recovery on reconnect only** (not the first connection): on reconnect, calls `mainClient.FetchRecentOrders(50)`, filters to orders with `OrderID > highestKnownID` in the local store, reverses them to oldest-first, and runs them through `orderService.Handle` (which prints + notifies + stores). First connection skips backlog because there's no session watermark yet.
- Event envelope is `order.OrderRequest` **plus** optional `_traceparent` / `_tracestate` fields. When present, the incoming trace context is `propagator.Extract`'d so the agent-side `sse.receive` span chains under main's `sse.publish` span — giving a single trace spanning both services. The fields are optional; omit-compatible with uninstrumented main.
- Scanner buffer is bumped to 1 MB/line — default 64 KB is too small for orders with many items.

**Trace propagation summary** (inbound *and* outbound):
- Inbound: `otelhttp.NewHandler` on the transport mux extracts `traceparent` from HTTP request headers (e.g. reprint requests from the mobile app if it adds them).
- Inbound SSE: extracted from the `_traceparent` field in the event JSON body (see above).
- Outbound HTTP (Pushover, mainclient backlog fetch, SSE dial): `otelhttp.NewTransport` wraps the clients.

**The `store` is a read-through cache for the mobile app**, not a source of truth. `main/` holds the authoritative 90-day history. `main` → `agent` drift during disconnects is reconciled by the backlog-recovery logic above.

**Pushover sounds are order-shaped signals** in `order/service.go`:
- `cash-order` when `Payments == nil` (no prepayment → expect cash on delivery/pickup)
- `obama-order` / `donald-order` randomly when `OrderTotal > 50` (big-ticket signal)
- `credit-order` otherwise

## Config surface

Required: `MAIN_SERVER_URL`, `AGENT_API_KEY`, `PRINTER_IP`.
Prod-only (5-printer mode): `PIZZA_PRINTER_IP`, `DESI_PRINTER_IP`, `SUB_PRINTER_IP`, `WINGS_PRINTER_IP`.
Optional: `HTTP_PORT` (8080), `PRINTER_DETECT_DELAY` (25 s), `LOG_LEVEL`, `LOG_OUTPUT` (`console`|`json`|`file`; `file` → lumberjack-rotated `log/agent.log`), `PUSHOVER_APP_TOKEN`, `PUSHOVER_USER_KEY`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME` (default `quicc-agent`).

`OTEL_EXPORTER_OTLP_ENDPOINT` accepts either a full URL (`https://otlp.quiccpos.com`) or a bare `host:port` — bare form defaults to `http://`. OTLP transport is **HTTP** here, not gRPC; don't point at `:4317`. Use `:4318` for local Collector or the Traefik-fronted `otlp.quiccpos.com`. Auth headers, if any, ride on the standard `OTEL_EXPORTER_OTLP_HEADERS` env var (e.g. `Authorization=Bearer <token>`).

## Web UI

`nginx/` serves a static `index.html` and proxies `/api/*` to the agent container over the compose-internal `agent-net`. `docker-compose.yml` publishes only nginx on port 80 — the agent itself is not port-exposed. Use `task devup` to bring both up.

## Sample receipt

`sample_receipt.html` at the repo root is a hand-made HTML mock of the thermal printout — handy for eyeballing formatting changes to `internal/infrastructure/printer/receipt/builder.go` without burning paper.
