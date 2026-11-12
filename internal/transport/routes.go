package transport

import (
	"net/http"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// routeDef is one HTTP route. It is the single source of truth: Start() builds
// the mux from this table, and GET /__routes serves its metadata (minus the
// handler) as JSON — so the reported routes can never drift from the registered
// ones. `Auth` selects the wrapper (see register) and is the label surfaced to
// the console routes viewer.
type routeDef struct {
	Method      string
	Path        string
	Auth        string // "none" (LAN-open) | "device" (paired tablet key OR session) | "session" (web-UI cookie) | "integration" (the store hub's key only) | "terminal" (a POS terminal's key OR session)
	Description string
	Handler     http.HandlerFunc
}

// RouteInfo is the JSON shape served at GET /__routes (shared shape across all
// modules: method, path, auth, description).
type RouteInfo struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Auth        string `json:"auth"`
	Description string `json:"description"`
}

// routes returns the full route table. Add/rename/re-gate a route HERE — the
// mux and the /__routes report both follow automatically.
func (s *Server) routes() []routeDef {
	return []routeDef{
		// Web UI — served by the agent binary itself (no nginx).
		{"GET", "/", "none", "Embedded web UI (SPA); 404s unmatched /api/* GETs", s.handleUI},
		{"GET", "/static/", "none", "Web UI assets (app.js/css, Alpine.js, fonts); 404 for a missing file or a directory", s.handleStatic},
		{"GET", "/api/map.pmtiles", "none", "The store's offline map (PMTiles, Range supported, max-age 1 day); 404 until downloaded", s.handleMapFile},
		{"OPTIONS", "/", "none", "CORS preflight for any route", s.handleCORSPreflight},

		// Introspection.
		{"GET", "/__routes", "none", "This route list (method/path/auth/description)", s.handleRoutes},
		{"GET", "/api/version", "none", "Build identifiers (version + commit)", s.handleVersion},

		// Provisioning + auth — LAN-open (these are the entry points before a
		// session exists). Each enforces its own state precondition server-side
		// (setup/token only when unconfigured, setup/password only when
		// needs_password) so they can't be driven out of order.
		{"POST", "/api/setup/token", "none", "Provision: verify + link an admin-minted token", s.handleSetupToken},
		{"POST", "/api/setup/link", "none", "Provision: link a blank Pi with the store manager login (restores the open batch, pairs the calling tablet)", s.handleSetupLink},
		{"POST", "/api/setup/password", "none", "Provision: set the local UI password (+optional hint), issue a session", s.handleSetupPassword},
		{"POST", "/api/login", "none", "Unlock the web UI with the local password (or the manager login on a linked Pi)", s.handleLogin},
		{"POST", "/api/logout", "none", "Clear the web-UI session cookie", s.handleLogout},
		{"GET", "/api/hint", "none", "The password hint (for the login 'Hint?' link)", s.handleHint},
		{"GET", "/api/session", "none", "Provisioning state (unconfigured|needs_password|locked|ready) + SSE health", s.handleSession},

		// Tablet pairing — open (needs a live 6-digit code from the web UI).
		{"POST", "/api/pair/manager", "none", "Join a linked Pi with the store manager login (2.4.2; no other tablet to get a code from) {username, password, tablet_name} -> 201 {device_id, name, kind, key, device_key} | 400 name_required | 401 bad_login | 409 not_linked/manager_login_unavailable | 429 too_many_attempts", s.handlePairManager},
		{"POST", "/api/pair", "none", "Pair a tablet, the store hub or a POS terminal: {code, name, kind?: tablet|integration|terminal} -> {device_id, name, kind, key, device_key} (shown once)", s.handlePair},

		// Floor — paired tablet (X-Agent-Device-Key) or web-UI session.
		{"GET", "/api/floor", "device", "Open batch, kitchen counter, prep quick-picks, sync backlog, delivery settings, store pin + map, settings {features, tickets, times}, integration (hub status)", s.handleFloor},
		{"GET", "/api/orders", "device", "Orders newest-first (?batch=current|all|<id>, default current)", s.handleListOrders},
		{"GET", "/api/orders/arrived", "device", "Orders awaiting acceptance (oldest-first)", s.handleListArrived},
		{"GET", "/api/orders/{id}", "device", "One local order by canonical UUID", s.handleGetOrder},
		{"POST", "/api/orders/{id}/accept", "device", "Accept {prep_minutes?}: kitchen #, print tickets (reverted if nothing printed)", s.handleAccept},
		{"POST", "/api/orders/{id}/ready", "device", "Mark an accepted order ready {callout?: false = READY held on the tablet, the hub doesn't call it out (2.4.0)}", s.handleReady},
		{"POST", "/api/orders/{id}/delivered", "device", "MARK DELIVERED (2.4.0): an open delivery of the open batch (cooking or ready, out of its pool) delivered with no driver - Unassigned on the Drivers tab -> StoredOrder | 404 | 409 invalid_state", s.handleMarkDelivered},
		{"POST", "/api/orders/{id}/undeliver", "device", "Back to deliveries (2.4.0): an Unassigned delivery back to cooking / ready -> StoredOrder | 404 | 409 not_unassigned", s.handleUndeliver},
		{"POST", "/api/orders/{id}/undo-ready", "device", "Undo a READY tapped by mistake (within 2 min, still ready): back to cooking, not finished; nothing sent to main or the hub -> StoredOrder | 404 | 409 undo_expired", s.handleUndoReady},
		{"POST", "/api/orders/{id}/reprint", "device", "Reprint the customer receipt only (no state change)", s.handleReprint},
		{"POST", "/api/orders/{id}/kitchen", "device", "Resend to kitchen: the station tickets only (no receipt, ignores pickup-cash) -> 200 {order, printed, failed, rerouted} | 409 not_accepted / nothing_for_kitchen | 502 print_failed | 404", s.handleResendKitchen},
		{"POST", "/api/orders/{id}/not-delivered", "device", "Not delivered {action: retry|cancel}: a delivery (on_delivery|delivered, open batch) leaves the driver's run -> retry = ready, no driver, came_back | cancel = cancelled -> StoredOrder | 400 bad_action | 404 | 409 not_out_or_delivered", s.handleNotDelivered},
		{"POST", "/api/orders/{id}/release", "device", "Release a came-back order: a normal unassigned ready delivery again -> StoredOrder | 404 | 409 not_came_back", s.handleReleaseCameBack},
		{"POST", "/api/deliveries/assign", "device", "Send ready deliveries out with a driver {driver_id, order_ids} (pre-1.4.0 Quibase; the orders leave their pools)", s.handleAssign},
		{"POST", "/api/deliveries/pool", "device", "Pool deliveries (accepted|ready) with an available driver {order_ids, driver_id|null = unassign}, all or nothing -> []StoredOrder | 400 | 404 not_found/driver_not_found | 409 invalid_state/driver_not_available", s.handlePool},
		{"POST", "/api/deliveries/reassign", "device", "Drivers tab (2.4.0): move delivered / out deliveries of the open batch {order_ids, driver_id: a clocked-in driver | \"\" = Unassigned (delivered only)}, all or nothing; reports follow, a POS check's driver changes through the hub -> []StoredOrder (moved) | 400 | 404 not_found/driver_not_found | 409 not_out_or_delivered/invalid_state/driver_not_available", s.handleReassign},
		{"POST", "/api/deliveries/auto-plan", "device", "AUTO: suggest runs for the available drivers (changes nothing; stored for confirm) -> {plan_id, runs, unassigned, ...} | 409 no_drivers", s.handleAutoPlan},
		{"POST", "/api/deliveries/auto-plan/{id}/confirm", "device", "AUTO confirm {order_ids?}: pool each still-valid suggestion (only those ids when given) -> {applied, skipped:[{order_id, reason}]} | 404", s.handleConfirmPlan},
		{"POST", "/api/drivers/{id}/send-out", "device", "Send the driver out with their pool {mark_ready, include_order_ids?, exclude_order_ids? (back to unassigned), plan_id?} -> []StoredOrder | 400 pool empty | 409 not_ready {order_ids}/driver_not_available", s.handleSendOut},
		{"GET", "/api/drivers/{id}/send-check", "device", "Before Send Out: orders far from the run (nearby rule) better held for a returning driver who still makes the target {?order_ids=a,b, default the pool} -> {hold:[{order_id, address, held_for:{driver_id, name, return_at}}]} (empty when fine; never blocks) | 404 driver_not_found", s.handleSendCheck},
		{"POST", "/api/drivers/{id}/pool/order", "device", "Stop order of the driver's pool (2.4.0, the tablet's up/down arrows) {order_ids: the whole pool in order}; kept until the pool's members change -> []StoredOrder (renumbered) | 400", s.handlePoolOrder},
		{"GET", "/api/drivers/{id}/pool/trip", "device", "The pool (or ?order_ids=a,b) in fastest driving order + route via main geo.trip (approximate local order when main is unreachable)", s.handlePoolTrip},
		{"POST", "/api/orders/{id}/pin", "device", "Drop a pin for a delivery address {lat, lng} -> RPC geo.pin -> 202 {} | 503 main_offline", s.handlePinOrder},
		{"POST", "/api/orders/{id}/geo/choose", "device", "Pick a candidate for an ambiguous address (geo state check) {index} -> RPC geo.choose -> 202 {} | 409 invalid_state | 503 main_offline", s.handleGeoChoose},
		{"POST", "/api/orders/{id}/geo/search", "device", "Search an address for a delivery {query 1-200 chars} -> RPC geo.search -> 202 {} (main pushes order.geo check with up to 5 places near the store, or not_found + note) | 400 bad_query | 404 | 429 too_soon | 503 main_offline | 501 main_unsupported", s.handleGeoSearch},
		{"POST", "/api/orders/{id}/make-now", "device", "Future order (2.4.3): MAKE NOW {prep_minutes 1-240} - out of the future and accepted at once (kitchen #, tickets + receipt with no FUTURE) -> StoredOrder | 409 not_future / invalid_state | 502 nothing_printed (it then rings as a new order)", s.handleMakeNow},
		{"POST", "/api/orders/{id}/due", "device", "Future order (2.4.3): CHANGE DUE TIME {promised_at RFC 3339, within 14 days} - the Pi's own due time (main keeps the vendor's), the ring time follows -> StoredOrder (status.due_moved_from) | 400 | 409 not_future / invalid_state", s.handleChangeDue},
		{"POST", "/api/orders/{id}/print-now", "device", "Future order (2.4.3): PRINT NOW {kitchen, receipt} - the early tickets (DO NOT MAKE / FUTURE) whatever the future switches -> {order, printed, failed, rerouted} | 400 | 409 not_future / nothing_for_kitchen | 502 print_failed", s.handlePrintNow},
		{"POST", "/api/orders/{id}/cancel", "device", "Future order (2.4.3): CANCEL a waiting future order (arrived or scheduled) -> StoredOrder (cancelled; main hears it) | 409 not_future / invalid_state", s.handleCancelFuture},
		{"POST", "/api/orders/{id}/tip", "device", "ADD TIP (2.5.0) on a card website order {tip_cents 0-50000: the whole tip added after the order} -> StoredOrder (status.added_tip_cents); main hears order.tip | 400 bad_request | 404 | 409 not_tippable (cash, POS/online, cancelled, closed batch) / tip_locked (card charged: driver clocked out or batch closed)", s.handleSetTip},
		{"POST", "/api/orders/{id}/cancel-order", "device", "CANCEL ORDER (2.5.0) on a website order not delivered / picked up {reason customer_asked|couldnt_deliver|other, note? <=140} -> StoredOrder (cancelled, status.cancel_reason; off its pool / run; VOID slip on the stations when it had printed); main hears order.status cancelled + reason | 400 bad_request | 404 | 409 not_cancellable (also an earlier batch's order or a picked-up pickup)", s.handleCancelOrder},
		{"GET", "/api/online", "device", "Online ordering (2.5.0, website + call center): main's online.status -> {state open|paused|closed, accepting, open_now, pause: null|{preset, until, note, ...}, closes_at, next_open_at} | 503 main_unreachable | 501 main_unsupported", s.handleOnlineStatus},
		{"POST", "/api/online/pause", "device", "PAUSE ONLINE (2.5.0) {preset 15m|30m|60m|today, note? <=140} -> main's online.pause -> the status | 400 | 503 main_unreachable", s.handleOnlinePause},
		{"DELETE", "/api/online/pause", "device", "Resume online ordering (2.5.0) -> main's online.resume -> the status | 503 main_unreachable", s.handleOnlineResume},
		{"GET", "/api/online/menu", "device", "Sold out (2.5.0): main's menu.stock -> {items:[{id, name, category, in_stock, out_until, hold}], groups:[{id, name, options:[{id, name, in_stock, hold}]}]} | 503 main_unreachable", s.handleMenuStock},
		{"POST", "/api/online/stock", "device", "Sold out (2.5.0) {kind item|option, id, in_stock, hold (until turned back on; else back at the new day)} -> main's menu.stock.set -> the row | 400 | 503 main_unreachable", s.handleSetStock},
		{"POST", "/api/orders/{id}/schedule", "device", "Future order: first confirm / edit the lead {lead_minutes 1-240} -> scheduled (rings again at promised_at - lead) | 409 not_future", s.handleSchedule},
		{"GET", "/api/deliveries/next-check", "device", "Nearby rule: is an unassigned delivery near the next (or ?driver_id=) driver's run? -> {driver_id, rule, nearby_order_ids, reason}", s.handleNextCheck},
		{"GET", "/api/settings/deliveries", "device", "Delivery settings {soon_ready_minutes, delivery_target_minutes, nearby_rule, nearby_detour_minutes, nearby_distance_mi, delivery_radius_m}", s.handleGetDeliveryPrefs},
		{"PUT", "/api/settings/deliveries", "device", "Delivery settings, any subset {soon_ready_minutes 0-30, delivery_target_minutes 15-120, nearby_rule detour|distance|bundle|off, nearby_detour_minutes 1-30, nearby_distance_mi 0.1-10, delivery_radius_m 500-80467 (-> main location.radius)} -> 200 all | 400 | 503 main_offline | 501 main_unsupported", s.audited("deliveries", s.readDeliveries, s.handleSetDeliveryPrefs)},
		{"GET", "/api/settings/auto-accept", "device", "Read the auto-accept toggle {auto_accept}", s.handleGetAutoAccept},
		{"PUT", "/api/settings/auto-accept", "device", "Write the auto-accept toggle {auto_accept}", s.audited("auto-accept", s.readAutoAccept, s.handleSetAutoAccept)},
		{"GET", "/api/settings/auto-update", "device", "Read the Pi auto-update toggle {auto_update} (update right after Close batch)", s.handleGetAutoUpdate},
		{"PUT", "/api/settings/auto-update", "device", "Write the Pi auto-update toggle {auto_update}", s.audited("auto-update", s.readAutoUpdate, s.handleSetAutoUpdate)},
		{"GET", "/api/dev/events", "device", "Developer mode: the Pi's recent events ?since=<seq> -> {events:[{seq, at, level, kind, text}], last_seq} (<=200, oldest first, no PII)", s.handleDevEvents},
		{"GET", "/api/settings/features", "device", "Feature toggles {future_orders, paid_wave}", s.handleGetFeatures},
		{"PUT", "/api/settings/features", "device", "Set feature toggles (missing keys keep their value) -> the toggles; SSE settings.updated", s.audited("features", s.readIntegration(func(v order.IntegrationSettings) any { return v.Features }), s.handleSetFeatures)},
		{"GET", "/api/settings/tickets", "device", "Kitchen-ticket switches {online_cash_pickup, online_cash_delivery, online_card, future_kitchen, future_receipt}", s.handleGetTickets},
		{"PUT", "/api/settings/tickets", "device", "Set kitchen-ticket switches (missing keys keep their value) -> the switches; SSE settings.updated", s.audited("tickets", s.readIntegration(func(v order.IntegrationSettings) any { return v.Tickets }), s.handleSetTickets)},
		{"GET", "/api/settings/finishing", "device", "Finishing orders {online_pickup_minutes 0-240 (0 = never)}: an online pickup is finished this long after ready; terminal checks finish when the POS closes them", s.handleGetFinishing},
		{"PUT", "/api/settings/finishing", "device", "Set finishing {online_pickup_minutes} -> the settings | 400", s.audited("finishing", s.readIntegration(func(v order.IntegrationSettings) any { return v.Finishing }), s.handleSetFinishing)},
		{"GET", "/api/settings/look", "device", "The store's colours (2.4.3) {accent, tickets: {delivery, pickup, counter}} as #RRGGBB", s.handleGetLook},
		{"PUT", "/api/settings/look", "device", "Set the store's colours (missing keys keep their value; #RRGGBB) -> the colours | 400; SSE settings.updated", s.audited("look", s.readIntegration(func(v order.IntegrationSettings) any { return v.Look }), s.handleSetLook)},
		{"GET", "/api/settings/callout", "device", "Order callout settings {enabled, voice, volume, music_dip, restore_seconds, chime, repeat, voice_lines, guest_chance, guest_voices}", s.handleGetCalloutSettings},
		{"PUT", "/api/settings/callout", "device", "Set the order callout (missing keys keep their value, voice_lines replaces the whole map; volume/music_dip 0-100, restore_seconds 0-10, repeat 1-3, guest_chance 0-50) -> the settings | 400; feed settings.changed", s.audited("callout", s.readIntegration(func(v order.IntegrationSettings) any { return v.Callout }), s.handleSetCalloutSettings)},
		{"GET", "/api/callout", "device", "Order callout: {settings, hub_connected, hub: {voices, default_voice, rendering, last, error} | null}", s.handleGetCallout},
		{"POST", "/api/callout/test", "device", "Speak a callout on the store speaker now {number 1-9999, voice?, kind?: test|preview} -> 202 {id, hub_connected} (feed callout.test)", s.handleCalloutTest},
		{"POST", "/api/callout/preview", "device", "PREVIEW on this tablet {number 1-9999, voice?} -> 202 {id, status: pending} (feed callout.render; the hub uploads the WAV) | 503 {code: hub_not_connected|hub_update_needed|not_installed}", s.handleCalloutPreview},
		{"GET", "/api/callout/preview/{id}", "device", "The preview's audio (?wait=<sec <=10>) -> 200 audio/wav | 202 {status: pending} | 404 | 503 {code: render_failed|not_installed} | 504 timeout (20 s)", s.handleCalloutPreviewAudio},
		{"GET", "/api/settings/changes", "device", "Settings history, newest first ?limit=N (default 100, max 500) -> {changes:[{id, at, section, by:{kind tablet|web, name, device_id?}, changes:[{key, from, to}]}], keep_days} (kept 90 days)", s.handleSettingsChanges},
		{"GET", "/api/settings/times", "device", "Default times {default_pickup_minutes, default_delivery_minutes}", s.handleGetTimes},
		{"PUT", "/api/settings/times", "device", "Set default times (each 1-240; missing keys keep their value) -> the times | 400", s.audited("times", s.readIntegration(func(v order.IntegrationSettings) any { return v.Times }), s.handleSetTimes)},
		{"GET", "/api/drivers", "device", "Driver roster in queue order (+ deliveries this batch)", s.handleListDrivers},
		{"POST", "/api/drivers", "device", "Add a driver {name, pushover_device?} (400 bad_device)", s.handleCreateDriver},
		{"POST", "/api/drivers/reorder", "device", "Set the queue order {ids}", s.handleReorderDrivers},
		{"PATCH", "/api/drivers/{id}", "device", "Rename a driver / set their Pushover device {name?, pushover_device? (\"\" clears)} (400 bad_device)", s.handleRenameDriver},
		{"POST", "/api/drivers/{id}/route/test", "device", "Send the driver's phone a sample route (Pushover) -> {ok} | 409 no_device | 502 device_not_found|pushover_rejected|pushover_unreachable | 503 not_configured", s.handleTestRoute},
		{"DELETE", "/api/drivers/{id}", "device", "Remove a driver", s.handleRemoveDriver},
		{"POST", "/api/drivers/{id}/clock-in", "device", "Clock a driver in: joins the back of the queue, shift opened", s.handleClockIn},
		{"POST", "/api/drivers/{id}/clock-out", "device", "Clock a driver out: leaves the queue, shift closed (409 while out on a run); prints their report in the background -> {...Driver, report_printing}; SSE driver.report_printed", s.handleClockOut},
		{"POST", "/api/drivers/{id}/report/print", "device", "Print the driver's report for the open batch so far (receipt printer) -> {driver_name, printed_on, report} | 502 nothing_printed", s.handlePrintDriverReport},
		{"GET", "/api/drivers/shifts", "device", "Driver shift records (?days=7), newest first", s.handleListShifts},
		{"GET", "/api/settings/driver-reset", "device", "Daily driver clock-out time {time, last_run}", s.handleGetDriverReset},
		{"POST", "/api/drivers/{id}/returned", "device", "Driver is back: their run is delivered, back of the queue", s.handleDriverReturned},
		{"POST", "/api/drivers/{id}/undo-return", "device", "Undo the latest return (10 min): driver back out at their old queue spot", s.handleUndoReturn},
		{"POST", "/api/drivers/{id}/recall", "device", "Bring a driver who is out back with his run (2.4.1): undelivered orders back in his pool (ready, run order), he is next in the queue, his phone told the run is on hold -> []StoredOrder | 404 driver_not_found | 409 driver_not_out", s.handleRecallDriver},
		{"POST", "/api/device/unpair", "device", "A paired tablet revokes its own key (204)", s.handleUnpairSelf},
		{"GET", "/api/batches", "device", "Open + kept closed batches, newest first", s.handleListBatches},
		{"POST", "/api/batch/close", "device", "Close the batch {force}: 409 lists leftovers unless force (prints driver reports); 409 unsettled_checks {checks} while an unpaid counter check is open (force doesn't pass it); answer adds unsettled {count, due_cents, checks} (unpaid pickups carried)", s.handleCloseBatch},
		{"GET", "/api/batches/{id}/driver-reports", "device", "Per-driver delivery/cash/tip reports for a batch (0 = open)", s.handleDriverReports},
		{"POST", "/api/batches/{id}/driver-reports/print", "device", "Print one driver-report slip per driver (receipt printer)", s.handlePrintDriverReports},
		{"GET", "/api/batches/{id}/z", "device", "A closed batch's Z report (contract §2.10l) -> {report} | 404 not_found (open, not built yet, or no longer kept)", s.handleBatchZ},
		{"GET", "/api/printers", "device", "Printer reachability snapshot (a POS terminal's key may read it too)", s.handleListPrinters},
		{"GET", "/api/printers/found", "device", "Printers seen on the LAN (TCP 9100) {scanned_at, scanning, printers:[{ip, mac, known, printer_name}]}; ?refresh=1 scans now", s.handlePrintersFound},
		{"POST", "/api/printers/add", "device", "Add a found printer {name, ip, mac} -> RPC printer.add -> 201 {id, name, ip} | 409 name_taken | 400 | 503 main_offline", s.handlePrinterAdd},
		{"GET", "/api/app/latest", "device", "Newest Quibase release cached for tablets (204 none)", s.handleAppLatest},
		{"GET", "/api/app/apk", "device", "Download the cached Quibase APK", s.handleAppAPK},
		{"GET", "/api/update/status", "device", "Pi update state: versions, latest, pending request, last result, can_update_now/blocked_reason", s.handleUpdateStatus},
		{"GET", "/api/events", "device", "SSE: order.arrived|updated|escalated, drivers.updated|dispatched, driver.route, batch.closed, floor.updated, settings.updated, integration.status, terminal.menu.changed|config.changed|order.placed|order.paid|order.voided|order.changed|order.picked_up|draft.changed|draft.removed|draft.taken|customer.changed|customers.synced|driver.linked|driver.claimed|driver.tip|driver.closed|driver.report|drawer.changed|z.report, sync.parked", s.handleEvents},

		// Quibase setup + diagnostics — paired tablet (or web-UI session).
		{"POST", "/api/pairing/code", "device", "Show a 6-digit tablet pairing code (5 min) — web UI or a paired tablet (Pair another tablet)", s.handlePairingCode},
		{"PATCH", "/api/device/self", "device", "A paired tablet renames itself {name} (1-40) -> {id, name}", s.handleRenameSelf},
		{"POST", "/api/device/report-token", "device", "Mint this tablet's diagnostics report token on main -> {token, main_url} (never stored here)", s.handleReportToken},
		{"GET", "/api/diag", "device", "Diagnostics snapshot: main link, Pi health, printers, orders, open_now, problems", s.handleDiag},
		{"POST", "/api/diag/report", "device", "Relay a tablet's diagnostics report (v1, <=256 KB) to main as RPC diag.report", s.handleDiagReport},
		{"POST", "/api/printers/test", "device", "Test print {printer_id?, probe_only?}: per-printer results, never fails over (409 while one runs)", s.handlePrinterTest},

		// Receipt settings (Quibase Settings -> Receipts) — paired tablet (or web-UI session).
		{"GET", "/api/settings/receipt", "device", "Receipt settings {prefs, defaults, footer_effective, footer_store_default, location_footer}", s.handleGetReceiptPrefs},
		{"PUT", "/api/settings/receipt", "device", "Save the receipt settings (full prefs; missing keys = defaults) -> {prefs, footer_effective, footer_store_default, location_footer}", s.audited("receipt", s.readReceipt, s.handleSetReceiptPrefs)},
		{"POST", "/api/printers/sample", "device", "Print a sample receipt + kitchen ticket with the current settings {printer_id?} (default the receipt printer), never fails over (409 while a test runs)", s.handlePrinterSample},

		// The store hub — its integration key only (tablet keys / sessions get 403).
		{"GET", "/api/integration/config", "integration", "What the hub (the callout box) needs: {location, callout, agent_version}", s.handleIntegrationConfig},
		{"PUT", "/api/integration/status", "integration", "Hub status {ok, insert_mode (the callout box sends off), operation_day, problems, version, callout} -> 204; silent 60 s = hub link down", s.handleIntegrationStatus},
		{"PUT", "/api/integration/callout-audio/{id}", "integration", "A callout.render's audio: body audio/wav (<= 2 MB) or JSON {error, code?: not_installed|render_failed} -> 204 | 404 unknown/expired | 400 not a WAV | 413", s.handleIntegrationCalloutAudio},
		{"GET", "/api/integration/events", "integration", "The hub feed (?after=<seq>&wait=<sec <=25>, long-poll) -> {events:[{seq,type,at,data}], next}; kept 3 days", s.handleIntegrationEvents},

		// POS terminals (terminal stage 0d) — a paired terminal's key (or the web-UI session).
		{"POST", "/api/terminal/login", "terminal", "Staff PIN login on this terminal {pin: 2-4 digits} -> {staff, role, limits, settings} | 400 bad_pin | 401 wrong_pin | 429 too_many_attempts | 503 config_unavailable", s.handleTerminalLogin},
		{"POST", "/api/terminal/manager-check", "terminal", "Check a manager's PIN for a terminal dialog {pin, action?: what it is for, default price_change} -> {ok: true, staff: {id, first_name, last_name, name, role}} | 400 bad_pin | 401 wrong_pin (wrong or not a manager's) / login_required | 429 too_many_attempts (counts with logins); journaled manager_check", s.handleTerminalManagerCheck},
		{"POST", "/api/terminal/logout", "terminal", "Lock: the staff member leaves this terminal -> 204", s.handleTerminalLogout},
		{"GET", "/api/terminal/session", "terminal", "Who is logged in here + the snapshot versions {staff|null, role, limits, menu_version, config_version, menu_fetched_at}", s.handleTerminalSession},
		{"GET", "/api/terminal/menu", "terminal", "The counter menu snapshot {menu_version, config_version, fetched_at, settings, counter_menu} (ETag / If-None-Match -> 304) | 503 menu_unavailable", s.handleTerminalMenu},
		{"POST", "/api/terminal/quote", "terminal", "Price a cart with the Pi's engine {type?: counter|pickup|delivery, cart: {lines (price_cents on a line = an item price change), code?, tip_cents?, discount?: {pct|amount_cents, reason}}} -> {quote, menu_version, discount_pct, needs_manager, price_changes?} | 400 | 503 menu_unavailable", s.handleTerminalQuote},
		{"POST", "/api/terminal/orders", "terminal", "Place a counter order (accepted at once, counter number, station tickets, synced as order.placed) {client_key, type, cart, customer?, payments?, note?, manager_pin?} -> 201 {order, number, duplicate:false, change_cents} | 200 the key's order (duplicate:true) | 400 | 401 login_required | 403 manager_required (discount / employee_meal / price_change over the limit, comp tender or $0 line)/manager_pin_invalid | 409 over_balance | 422 cart_invalid | 503 menu_unavailable", s.handleTerminalPlace},
		{"GET", "/api/terminal/orders", "terminal", "The open batch's counter orders, oldest first (?open=1: the Pick Up list - unpaid or not picked up)", s.handleTerminalOrders},
		{"GET", "/api/terminal/orders/{id}", "terminal", "One counter order, or an online / website pickup of the open batch, with its payments {order, payments} | 404", s.handleTerminalOrder},
		{"POST", "/api/terminal/orders/{id}/payments", "terminal", "Take a payment on a counter order or a cash-due online pickup {tender, amount_cents, tip_cents?, tendered_cents?, client_key?, ref?, last4?, manager_pin? (comp needs a manager)} or give money back {refund: true, reason, manager_pin?} -> {order, payment, duplicate, balance_cents} (order.payment to main) | 400 | 401 login_required | 403 manager_required | 404 | 409 over_balance/refund_too_big/not_payable", s.handleTerminalPayment},
		{"POST", "/api/terminal/refunds", "terminal", "A general refund (contract 2.10x): cash back with no order {client_key (required), amount_cents, reason (required), tender?: cash (only), manager_pin?, approvals?}: a cashier up to the cashier refund limit (default $20) per refund, above it a manager; in this terminal's drawer, the till / Z Refunds as General refund, refund.general to main -> {payment, duplicate, kick_drawer, staff_name, approver_name?} | 400 | 401 login_required | 403 manager_required | 409 key_reused", s.handleTerminalGeneralRefund},
		{"POST", "/api/terminal/orders/{id}/void", "terminal", "Void an unpaid counter order (or a carried one) {reason}: any staff, a VOID ticket (its lines under REMOVE / QUITAR) on every station it went to, order.void {kind: void} to main -> {order, refunds: [], refunded_cents: 0, already} | 400 | 401 | 404 | 409 refund_first {paid_cents} / not_voidable", s.handleTerminalVoid},
		{"POST", "/api/terminal/orders/{id}/waste", "terminal", "Void waste (contract 2.10s): throw an unpaid made order away {reason: one of the store's waste_reasons}: nothing prints, journal void.waste, the Z / till Waste section, order.void {kind: waste} -> {order, already} | 400 bad_reason {reasons} | 401 | 404 | 409 refund_first {paid_cents} / not_voidable", s.handleTerminalWaste},
		{"GET", "/api/terminal/unsettled", "terminal", "The Unsettled Check List (contract 2.10s): the open batch's checks still owing money + the carried ones {batch_id, generated_at, count, due_cents, blocking, checks: [{order_id, number, type, source, state, placed_at, batch_id, carried, customer, staff_id, staff_name, total_cents, paid_cents, due_cents, picked_up, blocks_close, items}]}", s.handleTerminalUnsettled},
		{"PUT", "/api/terminal/orders/{id}/cart", "terminal", "Change a placed counter order (kitchen change after SEND) {cart, client_key (required), manager_pin?, type?: pickup|delivery (moves takeout <-> delivery; a delivery needs customer.address), customer?, prep_minutes?}: re-priced, a change ticket per station (ADD / REMOVE; NOW <TYPE> (WAS <TYPE>) on a move), order.changed to main -> {order, added, removed, changed, type_changed, duplicate, balance_cents} | 400 | 401 login_required | 403 manager_required | 404 | 409 not_editable/refund_first/type_locked | 422 cart_invalid", s.handleTerminalChangeCart},
		// The Pickup list + shared drafts (§2.10i).
		{"GET", "/api/terminal/pickups", "terminal", "The Pickup list: the open batch's pickup / counter orders of every source (counter, phone, online, website; never POS checks) not picked up or still owing, oldest first {orders: [StoredOrder]} (status.cash_due_cents / paid = what is left to collect)", s.handleTerminalPickups},
		{"POST", "/api/terminal/orders/{id}/picked-up", "terminal", "A paid pickup (any source) handed over: finished_at set (an accepted one made ready), order.status to main, SSE terminal.order.picked_up -> {order, already} | 401 login_required | 404 | 409 cash_due {balance_cents} / not_pickable", s.handleTerminalPickedUp},
		{"GET", "/api/terminal/drafts", "terminal", "The store's shared drafts (held or open on a terminal) {drafts: [{id, type, name, held, holder|null, staff_id, staff_name, order_id?, draft, created_at, updated_at}]}", s.handleTerminalDrafts},
		{"PUT", "/api/terminal/drafts/{id}", "terminal", "Hold or open a shared draft {type, name, held, order_id?, take_over?, draft: object <= 64 KB} -> {draft} (holder = this terminal + its login when not held; SSE terminal.draft.changed, terminal.draft.taken {id, by} on a take-over) | 400 | 401 login_required | 409 open_elsewhere {holder} / too_many_drafts", s.handleTerminalPutDraft},
		{"DELETE", "/api/terminal/drafts/{id}", "terminal", "Drop a shared draft (sent, paid or cleared) -> 204 (SSE terminal.draft.removed {id}) | 401 login_required | 409 open_elsewhere {holder}", s.handleTerminalDeleteDraft},
		// Cash drawers (stage 2, contract §2.10k): one session per terminal + cashier shift.
		{"GET", "/api/terminal/drawer", "terminal", "This terminal's open drawer {drawer|null, mine|null (the logged-in cashier's open drawer on another terminal), device_id, staff|null, payout_limit_cents|null}; drawer = {id, device_id, device_name, staff_id, staff_name, batch_id, opened_at, start_cents, state, expected_cents, cash_sales_cents, cash_tips_cents, cash_refunds_cents, pay_ins_cents, pay_outs_cents, payments, no_sales, movements, ...}", s.handleTerminalDrawer},
		{"GET", "/api/terminal/drawers", "terminal", "A batch's drawer sessions ?batch=<id>|current (default current) -> {drawers: [Drawer]}", s.handleTerminalDrawers},
		{"POST", "/api/terminal/drawer/open", "terminal", "Count the logged-in cashier's drawer in on this terminal {start_cents | denominations: [{value_cents, count}]} -> 201 {drawer} (drawer.opened to main) | 400 | 401 login_required | 403 drawer_not_allowed (driver) | 409 drawer_open {drawer} / cashier_has_drawer {drawer}", s.handleTerminalDrawerOpen},
		{"POST", "/api/terminal/drawer/pay-in", "terminal", "Put cash in this terminal's drawer {amount_cents 1-1000000, reason, client_key?} -> {drawer, movement, duplicate} (drawer.movement) | 400 | 401 | 409 no_drawer / client_key_reused", s.drawerMove(terminal.MovePayIn)},
		{"POST", "/api/terminal/drawer/pay-out", "terminal", "Take cash out of this terminal's drawer {amount_cents, reason, client_key?, manager_pin?} (a cashier over cashier_payout_cents needs a manager) -> {drawer, movement, duplicate} | 400 | 401 | 403 manager_required {action: pay_out} / manager_pin_invalid | 409 no_drawer / not_enough_cash {expected_cents}", s.drawerMove(terminal.MovePayOut)},
		{"POST", "/api/terminal/drawer/close", "terminal", "Count a drawer out {counted_cents | denominations, drawer_id? (default this terminal's), manager_pin? (another cashier's drawer)} -> {drawer: {..., state: closed, close_reason: counted, counted_cents, expected_cents, over_short_cents}} (drawer.closed) | 400 | 401 | 403 manager_required {action: drawer_close} | 404 drawer_not_found | 409 no_drawer / drawer_closed", s.handleTerminalDrawerClose},
		{"POST", "/api/terminal/drawer/handover", "terminal", "The logged-in cashier takes over this terminal's drawer: counted out for the cashier before (close_reason handover) and counted in for them from the count {counted_cents | denominations} -> {closed, drawer} | 400 | 401 | 409 no_drawer / same_cashier / cashier_has_drawer", s.handleTerminalDrawerHandover},
		{"GET", "/api/terminal/z", "terminal", "The Z report (stage 2, contract §2.10l) ?batch=<id> -> {report} | 404 not_found; no batch = the kept ones {reports: [Z]} newest first", s.handleTerminalZ},
		{"GET", "/api/terminal/reports/batches", "terminal", "Manager tab (contract §2.10m): the open + kept closed batches, newest first {batches} | 401 login_required | 403 driver_not_allowed", s.handleTerminalReportBatches},
		{"GET", "/api/terminal/reports/till", "terminal", "Till report (SoftTouch's TILL REPORT) ?terminal=<device id>[&batch=] | ?batch=<id>|current | ?drawer=<id> -> {report: {scope, state, store, batch, drawer, summary, detail}} | 400 | 401 | 403 driver_not_allowed | 404 not_found", s.handleTerminalTill},
		{"GET", "/api/terminal/reports/drivers", "terminal", "A batch's driver reports ?batch=<id>|current -> {batch_id, reports: [DriverReport]} | 401 | 403 driver_not_allowed | 404 not_found", s.handleTerminalDriverReports},
		{"POST", "/api/terminal/reports/drivers/print", "terminal", "Print driver reports on the Pi's receipt printer {batch?, driver_id? (absent = every driver + Unassigned)} -> {batch_id, printed, failed} | 404 | 502 nothing_printed | 503 printing_unavailable", s.handleTerminalPrintDriverReports},
		{"POST", "/api/terminal/orders/{id}/payments/{payment_id}/tip", "terminal", "Set a card payment's whole tip afterwards {tip_cents 0-100000, client_key?} (contract §2.10p): the difference is a tip payment (amount 0, ref = the payment) -> {order, payment, tip_cents, duplicate} | 404 | 409 not_tippable / not_payable", s.handleTerminalPaymentTip},
		{"GET", "/api/terminal/checks", "terminal", "All Checks Overview (contract §2.10o): every order the Pi holds, filtered ?batch=<id>|current|all&from&to&type&source&state&paid&staff_id&device_id&tender&min_cents&max_cents&q&limit&offset -> {checks, total, limit, offset, batch_ids} | 400 | 401 | 403 driver_not_allowed", s.handleTerminalChecks},
		{"GET", "/api/terminal/checks/{id}", "terminal", "One order in full {check, order, payments, adjustments, journal} | 404 not_found", s.handleTerminalCheck},
		{"POST", "/api/terminal/close", "terminal", "The day's close (Close batch) from a terminal {force?, manager_pin?}: a manager logged in or a manager's PIN -> {closed, opened, auto_finished, unsettled: {count, due_cents, checks}} (the terminal prints the list; unpaid pickups are carried, still payable) | 401 | 403 manager_required {action: close_batch} / manager_pin_invalid | 409 unsettled_checks {checks} (an unpaid counter check: pay or void it) / batch_has_leftovers", s.handleTerminalClose},
		{"POST", "/api/terminal/journal", "terminal", "Journal a terminal-only action {action (not a Pi one), order_id?, detail?: object <= 4 KB} -> 201 the entry (journal.entries to main) | 400 | 401 login_required", s.handleTerminalJournalPost},
		// The store's customers (terminal 1d, contract §2.10j): a copy of main's, kept by its change feed.
		{"GET", "/api/terminal/customers", "terminal", "Look a caller up by phone (works offline) ?phone=<digits>: 10 digits = that number, 3-9 = numbers starting or ending with them (the last 4) -> {customers: [{id, phone, first_name, last_name, name, notes, source, last_order_at?, pending, addresses: [{id, label, line1, line2, city, state, zip, notes, default, area?: {state: in|near|out|unknown, distance_mi?}}]}], area: {radius_mi, margin_mi}|null, count, synced_at} (fewer than 3 digits: no customers) | 400", s.handleTerminalCustomers},
		{"POST", "/api/terminal/customers", "terminal", "Add a customer {phone, first_name, last_name?, notes?, address?: {line1, line2?, city, state?, zip?, notes?, label?}} -> 201 {customer, created: true, address_id?, area} | 200 created: false (the phone's customer: address added, only an empty name / notes filled); saved at once, customer.upsert to main, SSE terminal.customer.changed | 400 | 401 login_required | 409 too_many_addresses", s.handleTerminalAddCustomer},
		{"PATCH", "/api/terminal/customers/{id}", "terminal", "Change a customer {first_name?, last_name?, notes?, address?: {id?: change that one, else add, line1, city, ...}} (the phone never changes) -> {customer, created: false, address_id?, area} | 400 | 401 | 404 customer_not_found | 409 too_many_addresses", s.handleTerminalEditCustomer},
		{"POST", "/api/terminal/customers/{id}/addresses", "terminal", "Add an address to a customer {line1, city, line2?, state?, zip?, notes?, label?} (the same place: its blanks filled) -> {customer, address_id, area} | 400 | 401 | 404 | 409 too_many_addresses", s.handleTerminalAddCustomerAddress},
		{"GET", "/api/terminal/customers/{id}/orders", "terminal", "The customer's recent orders the Pi holds (open batch + kept closed ones; by customer id or phone, any source), newest first ?limit=N (default 10, max 50) -> {orders: [StoredOrder], source: pi} | 404", s.handleTerminalCustomerOrders},
		{"GET", "/api/terminal/addresses", "terminal", "Address suggestions as you type ?q= (an open-data index for the delivery area): not built yet -> 501 not_implemented", s.handleTerminalAddresses},
		{"GET", "/api/terminal/status", "terminal", "The terminal's alerts: {printers: [GET /api/printers' entries], printers_down, main: {state, connected, connected_since, last_event_at, last_error?}, menu_version, menu_fetched_at, config_version, at} (no login needed)", s.handleTerminalStatus},
		{"GET", "/api/terminal/drivers", "terminal", "Dispatch bar (contract 2.10v): the clocked-in drivers (available | out) in queue order {drivers: [Driver]} | 401 login_required", s.handleTerminalDrivers},
		{"POST", "/api/terminal/deliveries/send-out", "terminal", "Dispatch bar: send a driver out {driver_id, order_ids (pooled with him first), mark_ready} with those + his pool, as the tablet's SEND OUT (journal delivery.send_out) -> {orders} | 400 | 401 login_required | 404 not_found/driver_not_found | 409 not_ready {order_ids}/driver_not_available/invalid_state", s.handleTerminalSendOut},
		{"POST", "/api/terminal/drivers/{id}/returned", "terminal", "Dispatch bar: the driver is back (his run delivered, back of the queue; journal driver.returned) -> {orders, auto_assigned_order_ids} | 401 | 404 driver_not_found | 409", s.handleTerminalDriverReturned},
		{"POST", "/api/terminal/orders/{id}/time", "terminal", "Pickup / Dispatch bar: move a cooking or ready order's ready time (a pickup's ready time, a delivery's time) {ready_at: RFC 3339, at most 24 h ahead}: order.status to main, order.updated to the tablets, SSE terminal.order.time, journal order.time -> {order, from, to} | 400 | 401 | 404 | 409 invalid_state", s.handleTerminalOrderTime},
		{"GET", "/api/terminal/app/latest", "terminal", "Terminal self-update (contract 2.10w): the newest terminal APK the Pi has cached {app, version_code, version_name, sha256, size, created_at, install_window} | 204 none", s.handleTerminalAppLatest},
		{"GET", "/api/terminal/app/apk", "terminal", "Download the cached terminal APK (Range; X-Sha256) | 404 not_found", s.handleTerminalAppAPK},
		{"GET", "/api/terminal/journal", "terminal", "The action journal, newest first ?limit=N (default 100, max 500)", s.handleTerminalJournal},
		// The driver view (a driver logged in on the terminal; other roles 403 driver_only).
		{"GET", "/api/terminal/driver/available", "terminal", "Dispatch: deliveries nobody took (no driver, no pool; cooking, ready or delivered) waiting past the store's driver_claim_after_minutes (default 45), longest waiting first [{order_id, number, state, street, address, since, waiting_minutes, ready_at, payment, cash_due_cents, total_cents}] | 401 login_required | 403 driver_only", s.handleDriverAvailable},
		{"POST", "/api/terminal/driver/claim", "terminal", "Claim deliveries from Dispatch {order_ids}: his and delivered (not a send-out; he never goes out), all or nothing, first come first served -> {driver, orders} | 400 | 403 driver_only | 404 | 409 taken {order_ids}", s.handleDriverClaim},
		{"GET", "/api/terminal/driver/mine", "terminal", "His orders {driver, out: [ticket], delivered: [ticket]} (ticket: number, name, phone, address, notes, geo, payment, tip so far)", s.handleDriverMine},
		{"POST", "/api/terminal/driver/orders/{id}/tip", "terminal", "Set the tip on one of his orders {tip_cents 0-50000, absolute}: website card = ADD TIP (raises the hold), cash/counter = a door tip on his report -> {ticket, kind: card|door} | 403 not_your_order | 409 not_tippable/tip_locked", s.handleDriverTip},
		{"POST", "/api/terminal/driver/orders/{id}/close", "terminal", "Close one of his out orders (delivered); his last one puts him back in the queue -> {ticket, returned, driver} | 403 not_your_order | 409 invalid_state", s.handleDriverClose},
		{"GET", "/api/terminal/driver/report", "terminal", "His driver report for the open batch so far (cash, tips, orders)", s.handleDriverReportGet},
		{"POST", "/api/terminal/driver/report", "terminal", "Print his driver report on the receipt printer -> {driver_id, driver_name, printed_on, report} | 502 nothing_printed | 503 printing_unavailable", s.handleDriverReportPrint},

		// Web-UI-only — session-gated.
		{"GET", "/api/devices", "session", "Paired tablets", s.handleListDevices},
		{"POST", "/api/devices/{id}/revoke", "session", "Revoke a paired tablet", s.handleRevokeDevice},
		{"POST", "/api/orders/{id}/resend", "session", "Re-run the full arrival pipeline for a local order", s.handleResendByID},
		{"GET", "/api/history", "session", "Full order history, proxied from main/", s.handleHistory},
		{"POST", "/api/print", "session", "Print a fetched snapshot's customer receipt (no persist / fan-out)", s.handlePrint},
		{"POST", "/api/resend", "session", "Full arrival pipeline for a fetched snapshot", s.handleResend},
		{"GET", "/api/store-config", "session", "Current store config, proxied from main/", s.handleGetStoreConfig},
		{"PUT", "/api/settings/floor", "session", "Set {green_till_minutes, tip_tax_percent}", s.audited("floor", s.readFloorPrefs, s.handleSetFloorPrefs)},
		// Floor sync dead letters (contract 2.4e).
		{"GET", "/api/sync/parked", "session", "Floor events main refused for good, parked so the queue moves on {parked: [{event_id, type, subject, result, refusals, first_refused_at, parked_at, last_error, at}], count, held (events waiting behind a parked one about the same subject), backlog, settings: {refusals, hours}}", s.handleSyncParked},
		{"POST", "/api/sync/parked/retry", "session", "Put parked events back in the queue at their old place {event_ids?: [...] (none = all)} -> {retried, view} | 404 not_parked; journal sync.retry", s.handleSyncRetry},
		{"POST", "/api/sync/parked/{id}/retry", "session", "Put one parked event back in the queue -> {retried: 1, view} | 404 not_parked", s.handleSyncRetry},
		{"GET", "/api/settings/sync", "session", "Floor sync parking thresholds {refusals, hours}", s.handleGetSyncPark},
		{"PUT", "/api/settings/sync", "session", "Set the parking thresholds {refusals? 1-1000 (default 8), hours? 1-168 (default 2)}: a kept event main refused that often, or that long since its first refusal, is parked -> {refusals, hours} | 400", s.audited("sync", s.readSyncPark, s.handleSetSyncPark)},
		{"PUT", "/api/settings/driver-reset", "session", "Set the daily driver clock-out time {time: HH:MM}", s.audited("driver-reset", s.readDriverReset, s.handleSetDriverReset)},
		{"POST", "/api/settings/password", "session", "Change the local UI password (current + new + hint)", s.handleChangePassword},
		{"POST", "/api/update", "session", "Update this Pi now (right after Close batch only) -> 202 {id, state} | 409 {code} | 501 not_supported", s.handleUpdate},
		{"POST", "/api/update/rollback", "session", "Roll this Pi back to the previous release (no open orders) -> 202 | 409 | 501", s.handleRollback},
		{"POST", "/api/settings/unlink", "session", "Revoke this device on main + wipe local token & password", s.handleUnlink},
	}
}

// register wires every routeDef into the mux, wrapping by Auth: protected
// (session), floor (device key or session) or withCORS (open).
func (s *Server) register(mux *http.ServeMux) {
	for _, r := range s.routes() {
		wrap := s.withCORS
		switch r.Auth {
		case "session":
			wrap = s.protected
		case "device":
			wrap = s.floor
		case "integration":
			wrap = s.integration
		case "terminal":
			wrap = s.terminal
		}
		mux.HandleFunc(r.Method+" "+r.Path, wrap(r.Handler))
	}
}

// handleCORSPreflight answers the OPTIONS catch-all (withCORS already writes the
// headers; this just closes with 204).
func (s *Server) handleCORSPreflight(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// handleRoutes serves the route table as JSON (single source of truth for the
// console's routes viewer).
func (s *Server) handleRoutes(w http.ResponseWriter, _ *http.Request) {
	defs := s.routes()
	out := make([]RouteInfo, 0, len(defs))
	for _, d := range defs {
		out = append(out, RouteInfo{Method: d.Method, Path: d.Path, Auth: d.Auth, Description: d.Description})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"module":  "agent",
		"version": s.version,
		"routes":  out,
	})
}
