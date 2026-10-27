package transport

import (
	"net/http"
)

// routeDef is one HTTP route. It is the single source of truth: Start() builds
// the mux from this table, and GET /__routes serves its metadata (minus the
// handler) as JSON — so the reported routes can never drift from the registered
// ones. `Auth` selects the wrapper (see register) and is the label surfaced to
// the console routes viewer.
type routeDef struct {
	Method      string
	Path        string
	Auth        string // "none" (LAN-open) | "device" (paired tablet key OR session) | "session" (web-UI cookie) | "integration" (the store hub's key only)
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
		{"POST", "/api/pair", "none", "Pair a tablet or the store hub: {code, name, kind?: tablet|integration} -> {device_id, name, kind, key, device_key} (shown once)", s.handlePair},

		// Floor — paired tablet (X-Agent-Device-Key) or web-UI session.
		{"GET", "/api/floor", "device", "Open batch, kitchen counter, prep quick-picks, sync backlog, delivery settings, store pin + map, settings {features, tickets, times}, integration (hub status)", s.handleFloor},
		{"GET", "/api/orders", "device", "Orders newest-first (?batch=current|all|<id>, default current)", s.handleListOrders},
		{"GET", "/api/orders/arrived", "device", "Orders awaiting acceptance (oldest-first)", s.handleListArrived},
		{"GET", "/api/orders/{id}", "device", "One local order by canonical UUID", s.handleGetOrder},
		{"POST", "/api/orders/{id}/accept", "device", "Accept {prep_minutes?}: kitchen #, print tickets (reverted if nothing printed)", s.handleAccept},
		{"POST", "/api/orders/{id}/ready", "device", "Mark an accepted order ready", s.handleReady},
		{"POST", "/api/orders/{id}/reprint", "device", "Reprint the customer receipt only (no state change)", s.handleReprint},
		{"POST", "/api/orders/{id}/kitchen", "device", "Resend to kitchen: the station tickets only (no receipt, ignores pickup-cash) -> 200 {order, printed, failed, rerouted} | 409 not_accepted / nothing_for_kitchen | 502 print_failed | 404", s.handleResendKitchen},
		{"POST", "/api/orders/{id}/not-delivered", "device", "Not delivered {action: retry|cancel}: a delivery (on_delivery|delivered, open batch) leaves the driver's run -> retry = ready, no driver, came_back | cancel = cancelled -> StoredOrder | 400 bad_action | 404 | 409 not_out_or_delivered", s.handleNotDelivered},
		{"POST", "/api/orders/{id}/release", "device", "Release a came-back order: a normal unassigned ready delivery again -> StoredOrder | 404 | 409 not_came_back", s.handleReleaseCameBack},
		{"POST", "/api/deliveries/assign", "device", "Send ready deliveries out with a driver {driver_id, order_ids} (pre-1.4.0 Quibase; the orders leave their pools)", s.handleAssign},
		{"POST", "/api/deliveries/pool", "device", "Pool deliveries (accepted|ready) with an available driver {order_ids, driver_id|null = unassign}, all or nothing -> []StoredOrder | 400 | 404 not_found/driver_not_found | 409 invalid_state/driver_not_available", s.handlePool},
		{"POST", "/api/deliveries/auto-plan", "device", "AUTO: suggest runs for the available drivers (changes nothing; stored for confirm) -> {plan_id, runs, unassigned, ...} | 409 no_drivers", s.handleAutoPlan},
		{"POST", "/api/deliveries/auto-plan/{id}/confirm", "device", "AUTO confirm {order_ids?}: pool each still-valid suggestion (only those ids when given) -> {applied, skipped:[{order_id, reason}]} | 404", s.handleConfirmPlan},
		{"POST", "/api/drivers/{id}/send-out", "device", "Send the driver out with their pool {mark_ready, include_order_ids?, exclude_order_ids? (back to unassigned), plan_id?} -> []StoredOrder | 400 pool empty | 409 not_ready {order_ids}/driver_not_available", s.handleSendOut},
		{"GET", "/api/drivers/{id}/send-check", "device", "Before Send Out: orders far from the run (nearby rule) better held for a returning driver who still makes the target {?order_ids=a,b, default the pool} -> {hold:[{order_id, address, held_for:{driver_id, name, return_at}}]} (empty when fine; never blocks) | 404 driver_not_found", s.handleSendCheck},
		{"GET", "/api/drivers/{id}/pool/trip", "device", "The pool (or ?order_ids=a,b) in fastest driving order + route via main geo.trip (approximate local order when main is unreachable)", s.handlePoolTrip},
		{"POST", "/api/orders/{id}/pin", "device", "Drop a pin for a delivery address {lat, lng} -> RPC geo.pin -> 202 {} | 503 main_offline", s.handlePinOrder},
		{"POST", "/api/orders/{id}/geo/choose", "device", "Pick a candidate for an ambiguous address (geo state check) {index} -> RPC geo.choose -> 202 {} | 409 invalid_state | 503 main_offline", s.handleGeoChoose},
		{"POST", "/api/orders/{id}/geo/search", "device", "Search an address for a delivery {query 1-200 chars} -> RPC geo.search -> 202 {} (main pushes order.geo check with up to 5 places near the store, or not_found + note) | 400 bad_query | 404 | 429 too_soon | 503 main_offline | 501 main_unsupported", s.handleGeoSearch},
		{"POST", "/api/orders/{id}/schedule", "device", "Future order: first confirm / edit the lead {lead_minutes 1-240} -> scheduled (rings again at promised_at - lead) | 409 not_future", s.handleSchedule},
		{"GET", "/api/deliveries/next-check", "device", "Nearby rule: is an unassigned delivery near the next (or ?driver_id=) driver's run? -> {driver_id, rule, nearby_order_ids, reason}", s.handleNextCheck},
		{"GET", "/api/settings/deliveries", "device", "Delivery settings {soon_ready_minutes, delivery_target_minutes, nearby_rule, nearby_detour_minutes, nearby_distance_mi, delivery_radius_m}", s.handleGetDeliveryPrefs},
		{"PUT", "/api/settings/deliveries", "device", "Delivery settings, any subset {soon_ready_minutes 0-30, delivery_target_minutes 15-120, nearby_rule detour|distance|bundle|off, nearby_detour_minutes 1-30, nearby_distance_mi 0.1-10, delivery_radius_m 500-80467 (-> main location.radius)} -> 200 all | 400 | 503 main_offline | 501 main_unsupported", s.handleSetDeliveryPrefs},
		{"GET", "/api/settings/auto-accept", "device", "Read the auto-accept toggle {auto_accept}", s.handleGetAutoAccept},
		{"PUT", "/api/settings/auto-accept", "device", "Write the auto-accept toggle {auto_accept}", s.handleSetAutoAccept},
		{"GET", "/api/settings/auto-update", "device", "Read the Pi auto-update toggle {auto_update} (update right after Close batch)", s.handleGetAutoUpdate},
		{"PUT", "/api/settings/auto-update", "device", "Write the Pi auto-update toggle {auto_update}", s.handleSetAutoUpdate},
		{"GET", "/api/dev/events", "device", "Developer mode: the Pi's recent events ?since=<seq> -> {events:[{seq, at, level, kind, text}], last_seq} (<=200, oldest first, no PII)", s.handleDevEvents},
		{"GET", "/api/settings/features", "device", "Feature toggles {pos_pull, order_insert, driver_writeback, address_heal, future_orders, paid_wave}", s.handleGetFeatures},
		{"PUT", "/api/settings/features", "device", "Set feature toggles (missing keys keep their value) -> the toggles; SSE settings.updated", s.handleSetFeatures},
		{"GET", "/api/settings/tickets", "device", "Kitchen-ticket switches {pos_delivery, pos_takeout, pos_counter, online_cash_pickup, online_cash_delivery, online_card}", s.handleGetTickets},
		{"PUT", "/api/settings/tickets", "device", "Set kitchen-ticket switches (missing keys keep their value) -> the switches; SSE settings.updated", s.handleSetTickets},
		{"GET", "/api/settings/times", "device", "Default times {default_pickup_minutes, default_delivery_minutes}", s.handleGetTimes},
		{"PUT", "/api/settings/times", "device", "Set default times (each 1-240; missing keys keep their value) -> the times | 400", s.handleSetTimes},
		{"GET", "/api/drivers", "device", "Driver roster in queue order (+ deliveries this batch)", s.handleListDrivers},
		{"POST", "/api/drivers", "device", "Add a driver {name, pos_number?} (409 pos_number_taken)", s.handleCreateDriver},
		{"POST", "/api/drivers/reorder", "device", "Set the queue order {ids}", s.handleReorderDrivers},
		{"PATCH", "/api/drivers/{id}", "device", "Rename a driver / set their POS number {name?, pos_number? (\"\" clears)} (409 pos_number_taken)", s.handleRenameDriver},
		{"DELETE", "/api/drivers/{id}", "device", "Remove a driver", s.handleRemoveDriver},
		{"POST", "/api/drivers/{id}/clock-in", "device", "Clock a driver in: joins the back of the queue, shift opened", s.handleClockIn},
		{"POST", "/api/drivers/{id}/clock-out", "device", "Clock a driver out: leaves the queue, shift closed (409 while out on a run)", s.handleClockOut},
		{"GET", "/api/drivers/shifts", "device", "Driver shift records (?days=7), newest first", s.handleListShifts},
		{"GET", "/api/settings/driver-reset", "device", "Daily driver clock-out time {time, last_run}", s.handleGetDriverReset},
		{"POST", "/api/drivers/{id}/returned", "device", "Driver is back: their run is delivered, back of the queue", s.handleDriverReturned},
		{"POST", "/api/drivers/{id}/undo-return", "device", "Undo the latest return (10 min): driver back out at their old queue spot", s.handleUndoReturn},
		{"POST", "/api/device/unpair", "device", "A paired tablet revokes its own key (204)", s.handleUnpairSelf},
		{"GET", "/api/batches", "device", "Open + kept closed batches, newest first", s.handleListBatches},
		{"POST", "/api/batch/close", "device", "Close the batch {force}: 409 lists leftovers unless force (prints driver reports)", s.handleCloseBatch},
		{"GET", "/api/batches/{id}/driver-reports", "device", "Per-driver delivery/cash/tip reports for a batch (0 = open)", s.handleDriverReports},
		{"POST", "/api/batches/{id}/driver-reports/print", "device", "Print one driver-report slip per driver (receipt printer)", s.handlePrintDriverReports},
		{"GET", "/api/printers", "device", "Printer reachability snapshot", s.handleListPrinters},
		{"GET", "/api/printers/found", "device", "Printers seen on the LAN (TCP 9100) {scanned_at, scanning, printers:[{ip, mac, known, printer_name}]}; ?refresh=1 scans now", s.handlePrintersFound},
		{"POST", "/api/printers/add", "device", "Add a found printer {name, ip, mac} -> RPC printer.add -> 201 {id, name, ip} | 409 name_taken | 400 | 503 main_offline", s.handlePrinterAdd},
		{"GET", "/api/printers/{name}", "device", "One printer's status", s.handleGetPrinter},
		{"GET", "/api/app/latest", "device", "Newest Quibase release cached for tablets (204 none)", s.handleAppLatest},
		{"GET", "/api/app/apk", "device", "Download the cached Quibase APK", s.handleAppAPK},
		{"GET", "/api/update/status", "device", "Pi update state: versions, latest, pending request, last result, can_update_now/blocked_reason", s.handleUpdateStatus},
		{"GET", "/api/events", "device", "SSE: order.arrived|added|updated|escalated, drivers.updated|dispatched, batch.closed, floor.updated, settings.updated, integration.status|toast", s.handleEvents},

		// Quibase setup + diagnostics — paired tablet (or web-UI session).
		{"POST", "/api/pairing/code", "device", "Show a 6-digit tablet pairing code (5 min) — web UI or a paired tablet (Pair another tablet)", s.handlePairingCode},
		{"PATCH", "/api/device/self", "device", "A paired tablet renames itself {name} (1-40) -> {id, name}", s.handleRenameSelf},
		{"POST", "/api/device/report-token", "device", "Mint this tablet's diagnostics report token on main -> {token, main_url} (never stored here)", s.handleReportToken},
		{"GET", "/api/diag", "device", "Diagnostics snapshot: main link, Pi health, printers, orders, open_now, problems", s.handleDiag},
		{"POST", "/api/diag/report", "device", "Relay a tablet's diagnostics report (v1, <=256 KB) to main as RPC diag.report", s.handleDiagReport},
		{"POST", "/api/printers/test", "device", "Test print {printer_id?, probe_only?}: per-printer results, never fails over (409 while one runs)", s.handlePrinterTest},

		// Receipt settings (Quibase Settings -> Receipts) — paired tablet (or web-UI session).
		{"GET", "/api/settings/receipt", "device", "Receipt settings {prefs, defaults, footer_effective, footer_store_default, location_footer}", s.handleGetReceiptPrefs},
		{"PUT", "/api/settings/receipt", "device", "Save the receipt settings (full prefs; missing keys = defaults) -> {prefs, footer_effective, footer_store_default, location_footer}", s.handleSetReceiptPrefs},
		{"POST", "/api/printers/sample", "device", "Print a sample receipt + kitchen ticket with the current settings {printer_id?} (default the receipt printer), never fails over (409 while a test runs)", s.handlePrinterSample},

		// The store hub — its integration key only (tablet keys / sessions get 403).
		{"GET", "/api/integration/config", "integration", "Location, features, tickets, times, drivers (pos_number), printers, agent_version", s.handleIntegrationConfig},
		{"PUT", "/api/integration/status", "integration", "Hub status {ok, insert_mode off|shadow|live, operation_day, problems, version} -> 204; silent 60 s = POS link down", s.handleIntegrationStatus},
		{"PUT", "/api/integration/orders/{ext_ref}", "integration", "Upsert a POS check (stored accepted, local only) -> {order_id, created, changed} | 409 pos_pull_off", s.handleIntegrationUpsert},
		{"POST", "/api/integration/orders/{ext_ref}/void", "integration", "A POS check voided at the terminal -> the cancelled order", s.handleIntegrationVoid},
		{"PATCH", "/api/integration/online-orders/{order_id}", "integration", "An online order the hub inserted / refused {paid?, pos_check_number?, flag?|null} -> StoredOrder", s.handleIntegrationPatchOnline},
		{"GET", "/api/integration/events", "integration", "The hub feed (?after=<seq>&wait=<sec <=25>, long-poll) -> {events:[{seq,type,at,data}], next}; kept 3 days", s.handleIntegrationEvents},

		// Web-UI-only — session-gated.
		{"GET", "/api/devices", "session", "Paired tablets", s.handleListDevices},
		{"POST", "/api/devices/{id}/revoke", "session", "Revoke a paired tablet", s.handleRevokeDevice},
		{"POST", "/api/orders/{id}/resend", "session", "Re-run the full arrival pipeline for a local order", s.handleResendByID},
		{"GET", "/api/history", "session", "Full order history, proxied from main/", s.handleHistory},
		{"POST", "/api/print", "session", "Print a fetched snapshot's customer receipt (no persist / fan-out)", s.handlePrint},
		{"POST", "/api/resend", "session", "Full arrival pipeline for a fetched snapshot", s.handleResend},
		{"GET", "/api/store-config", "session", "Current store config, proxied from main/", s.handleGetStoreConfig},
		{"PUT", "/api/settings/floor", "session", "Set {green_till_minutes, tip_tax_percent}", s.handleSetFloorPrefs},
		{"PUT", "/api/settings/driver-reset", "session", "Set the daily driver clock-out time {time: HH:MM}", s.handleSetDriverReset},
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
