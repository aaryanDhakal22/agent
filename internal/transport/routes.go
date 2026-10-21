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
	Auth        string // "none" (LAN-open) | "device" (paired tablet key OR session) | "session" (web-UI cookie)
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
		{"POST", "/api/pair", "none", "Pair a tablet: {code, name} -> device key (shown once)", s.handlePair},

		// Floor — paired tablet (X-Agent-Device-Key) or web-UI session.
		{"GET", "/api/floor", "device", "Open batch, kitchen counter, prep quick-picks, sync backlog", s.handleFloor},
		{"GET", "/api/orders", "device", "Orders newest-first (?batch=current|all|<id>, default current)", s.handleListOrders},
		{"GET", "/api/orders/arrived", "device", "Orders awaiting acceptance (oldest-first)", s.handleListArrived},
		{"GET", "/api/orders/{id}", "device", "One local order by canonical UUID", s.handleGetOrder},
		{"POST", "/api/orders/{id}/accept", "device", "Accept {prep_minutes?}: kitchen #, print tickets (reverted if nothing printed)", s.handleAccept},
		{"POST", "/api/orders/{id}/ready", "device", "Mark an accepted order ready", s.handleReady},
		{"POST", "/api/orders/{id}/reprint", "device", "Reprint the customer receipt only (no state change)", s.handleReprint},
		{"POST", "/api/orders/{id}/kitchen", "device", "Resend to kitchen: the station tickets only (no receipt, ignores pickup-cash) -> 200 {order, printed, failed, rerouted} | 409 not_accepted / nothing_for_kitchen | 502 print_failed | 404", s.handleResendKitchen},
		{"POST", "/api/deliveries/assign", "device", "Send ready deliveries out with a driver {driver_id, order_ids}", s.handleAssign},
		{"GET", "/api/drivers", "device", "Driver roster in queue order (+ deliveries this batch)", s.handleListDrivers},
		{"POST", "/api/drivers", "device", "Add a driver {name}", s.handleCreateDriver},
		{"POST", "/api/drivers/reorder", "device", "Set the queue order {ids}", s.handleReorderDrivers},
		{"PATCH", "/api/drivers/{id}", "device", "Rename a driver {name}", s.handleRenameDriver},
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
		{"GET", "/api/events", "device", "SSE: order.arrived|updated|escalated, drivers.updated|dispatched, batch.closed", s.handleEvents},

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
		{"GET", "/api/settings/auto-accept", "session", "Read the auto-accept toggle", s.handleGetAutoAccept},
		{"PUT", "/api/settings/auto-accept", "session", "Write the auto-accept toggle", s.handleSetAutoAccept},
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
