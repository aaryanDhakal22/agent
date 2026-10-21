package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/application/health"
	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/storeconfig"
	agentrpc "quiccpos/agent/internal/rpc"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Diagnostics + tablet setup support for Quibase (agent 2.0.1): GET /api/diag
// (one snapshot the tablet's troubleshooting guides rank causes from), printer
// test print / probe (never fails over), tablet self-rename, the tablet's
// report token (proxied from main, never stored here) and a diagnostics
// report relayed to main over the duplex channel. All device-auth routes.

const (
	// maxDiagReport caps POST /api/diag/report (main enforces the same).
	maxDiagReport = 256 << 10
	// defaultQuietAlertMinutes: "no orders for this long while open" is worth
	// a look (location config quiet_alert_minutes overrides).
	defaultQuietAlertMinutes = 90
	// main reachability probe: GET {MAIN}/health, cached so a tablet polling
	// /api/diag doesn't turn into a stream of requests to main.
	mainProbeTimeout = 3 * time.Second
	mainProbeTTL     = 20 * time.Second
	diagReportWait   = 10 * time.Second
)

// DiagDeps wires the diagnostics surface (SetDiag in cmd/main.go). Every field
// is optional: a missing one just leaves that part of /api/diag empty.
type DiagDeps struct {
	Dispatcher  *printerApp.Dispatcher
	Health      *health.Heartbeat
	StoreCtx    func() storeconfig.StoreContext
	RPC         *agentrpc.Registry
	AcksPending func() int
	// TestSlip renders the test slip for one printer (receipt.BuildTestSlip
	// with the live store context).
	TestSlip func(printerName, tablet string, at time.Time) []byte
}

// SetDiag wires the diagnostics routes.
func (s *Server) SetDiag(d DiagDeps) { s.diag = d }

func (s *Server) storeContext() storeconfig.StoreContext {
	if s.diag.StoreCtx == nil {
		return storeconfig.StoreContext{}
	}
	return s.diag.StoreCtx()
}

// mainProbe caches the live main reachability check.
type mainProbe struct {
	mu sync.Mutex
	at time.Time
	ok bool
}

func (p *mainProbe) reachable(ctx context.Context, baseURL string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && time.Since(p.at) < mainProbeTTL {
		return p.ok
	}
	ctx, cancel := context.WithTimeout(ctx, mainProbeTimeout)
	defer cancel()
	p.ok = false
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil); err == nil {
		hc := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
		if resp, err := hc.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			p.ok = resp.StatusCode < 500
		}
	}
	p.at = time.Now()
	return p.ok
}

// requestDevice is the paired tablet behind a device-auth request (nil for a
// web-UI session).
func requestDevice(r *http.Request) *device.Device {
	d, _ := r.Context().Value(deviceCtxKey{}).(*device.Device)
	return d
}

// --- GET /api/diag ---------------------------------------------------------

type diagMain struct {
	State          string     `json:"state"` // connected | connecting | rejected | unconfigured
	ConnectedSince *time.Time `json:"connected_since"`
	LastEventAt    *time.Time `json:"last_event_at"`
	LastError      string     `json:"last_error"`
	Reconnects     int64      `json:"reconnects"`
	Reachable      bool       `json:"reachable"`
}

type diagHealth struct {
	health.SystemHealth
	DiskFreeBytes uint64  `json:"disk_free_bytes"`
	TempC         float64 `json:"temp_c,omitempty"`
}

type diagPrinter struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	IP          string     `json:"ip"`
	Up          bool       `json:"up"`
	IsReceipt   bool       `json:"is_receipt"`
	LastError   string     `json:"last_error"`
	LastChecked *time.Time `json:"last_checked"`
	DownSince   *time.Time `json:"down_since"`
}

type diagOrders struct {
	LastArrivedAt   *time.Time `json:"last_arrived_at"`
	OpenBatchOrders int64      `json:"open_batch_orders"`
	Waiting         int        `json:"waiting"`
}

type diagResponse struct {
	Version           string        `json:"version"`
	Hostname          string        `json:"hostname"`
	StoreName         string        `json:"store_name"`
	UptimeS           int64         `json:"uptime_s"`
	Main              diagMain      `json:"main"`
	Health            diagHealth    `json:"health"`
	Problems          []string      `json:"problems"`
	Printers          []diagPrinter `json:"printers"`
	Orders            diagOrders    `json:"orders"`
	OpenNow           *bool         `json:"open_now"`
	QuietAlertMinutes int           `json:"quiet_alert_minutes"`
	FloorBacklog      int64         `json:"floor_backlog"`
	AcksPending       int           `json:"acks_pending"`
}

// sseState is the main-connection verdict shared by /api/session and /api/diag.
func (s *Server) sseState(configured bool) string {
	if !configured || s.sseController == nil {
		return "unconfigured"
	}
	st := s.sseController.Status()
	switch {
	case st.Connected:
		return "connected"
	case st.AuthRejected:
		return "rejected"
	default:
		return "connecting"
	}
}

func unixPtr(sec int64) *time.Time {
	if sec <= 0 {
		return nil
	}
	t := time.Unix(sec, 0).UTC()
	return &t
}

// GET /api/diag — one snapshot of everything the tablet's guides check.
func (s *Server) handleDiag(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sc := s.storeContext()
	host, _ := os.Hostname()
	out := diagResponse{Version: s.version, Hostname: host, StoreName: sc.StoreName(),
		UptimeS: int64(time.Since(s.started).Seconds()), Problems: []string{}, Printers: []diagPrinter{},
		QuietAlertMinutes: sc.LocationConfigInt("quiet_alert_minutes", defaultQuietAlertMinutes)}

	// main: the SSE connection + a live reachability check.
	key, _ := s.settings.GetStoreAPIKey(ctx)
	out.Main.State = s.sseState(key != "")
	if s.sseController != nil {
		st := s.sseController.Status()
		out.Main.ConnectedSince, out.Main.LastEventAt = unixPtr(st.ConnectedSince), unixPtr(st.LastEventAt)
		out.Main.LastError, out.Main.Reconnects = st.LastError, st.Reconnects
	}
	if s.mainClient != nil {
		out.Main.Reachable = s.mainReach.reachable(ctx, s.mainClient.BaseURL())
	}

	// The Pi itself.
	if s.diag.Health != nil {
		rep := s.diag.Health.Snapshot(ctx)
		out.Health = diagHealth{SystemHealth: rep.Pi, DiskFreeBytes: rep.DiskFreeBytes, TempC: rep.TempC}
	} else {
		out.Health.FileStale = true
	}

	// Printers, in roster order, with the health check's last verdict.
	if s.diag.Dispatcher != nil {
		for _, e := range s.diag.Dispatcher.Roster() {
			p := diagPrinter{ID: e.ID, Name: e.Name, IsReceipt: e.IsReceipt}
			if st, ok := s.printers.Get(e.Name); ok {
				p.IP, p.Up, p.LastError, p.LastChecked = st.IP, st.Up, st.LastError, st.LastChecked
				if !st.Up && st.LastChecked != nil {
					p.DownSince = st.DownSince
				}
			}
			out.Printers = append(out.Printers, p)
		}
	}

	// Orders + the floor outbox.
	if s.service != nil {
		if latest, err := s.service.ListPage(ctx, 0, 1); err == nil && len(latest) > 0 {
			at := latest[0].Status.ArrivalDate.UTC()
			out.Orders.LastArrivedAt = &at
		}
		if f, err := s.service.Floor(ctx); err == nil {
			out.Orders.OpenBatchOrders, out.FloorBacklog = f.Batch.OrderCount, f.UnsyncedEvents
		}
		if waiting, err := s.service.ListArrived(ctx); err == nil {
			out.Orders.Waiting = len(waiting)
		}
	}
	if s.diag.AcksPending != nil {
		out.AcksPending = s.diag.AcksPending()
	}
	if open, known := sc.OpenNow(time.Now()); known {
		out.OpenNow = &open
	}

	out.Problems = diagProblems(out)
	s.writeJSON(w, http.StatusOK, out)
}

// diagProblems lists short codes for what's wrong, most physical first. A
// missing health file (dev box) isn't a problem; an old one is.
func diagProblems(d diagResponse) []string {
	p := []string{}
	h := d.Health
	if h.SDErrors != nil && *h.SDErrors > 0 {
		p = append(p, "sd_errors")
	}
	if h.RootfsRO != nil && *h.RootfsRO {
		p = append(p, "rootfs_ro")
	}
	if h.UnderVoltageNow || h.UnderVoltageSinceBoot {
		p = append(p, "under_voltage")
	}
	if h.NTPSynced == "no" {
		p = append(p, "ntp")
	}
	if h.FilePresent && h.FileStale {
		p = append(p, "health_stale")
	}
	if d.Main.State != "unconfigured" && d.Main.State != "connected" {
		p = append(p, "main_down")
	}
	for _, pr := range d.Printers {
		if !pr.Up && pr.LastChecked != nil {
			p = append(p, "printer_down")
			break
		}
	}
	return p
}

// --- POST /api/printers/test -----------------------------------------------

// POST /api/printers/test {printer_id?, probe_only?} — a test slip on every
// printer (or one), each only on itself; probe_only re-checks reachability
// without printing. 409 test_in_progress while another test runs.
func (s *Server) handlePrinterTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PrinterID string `json:"printer_id"`
		ProbeOnly bool   `json:"probe_only"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	if s.diag.Dispatcher == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("printing isn't available", "not_available"))
		return
	}
	if !s.testMu.TryLock() {
		s.writeJSON(w, http.StatusConflict, errCodeMap("A printer test is already running — wait for it.", "test_in_progress"))
		return
	}
	defer s.testMu.Unlock()

	id := strings.TrimSpace(req.PrinterID)
	var results []printerApp.TestResult
	if req.ProbeOnly {
		results = s.diag.Dispatcher.Probe(r.Context(), id)
	} else {
		tablet := "Web UI"
		if d := requestDevice(r); d != nil {
			tablet = d.Name
		}
		now := time.Now()
		results = s.diag.Dispatcher.PrintTest(r.Context(), id, func(e printerApp.RosterEntry) []byte {
			if s.diag.TestSlip == nil {
				return nil
			}
			return s.diag.TestSlip(e.Name, tablet, now)
		})
	}
	if results == nil {
		results = []printerApp.TestResult{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// --- PATCH /api/device/self ------------------------------------------------

// PATCH /api/device/self {name} — the calling tablet renames itself (1-40).
func (s *Server) handleRenameSelf(w http.ResponseWriter, r *http.Request) {
	d := requestDevice(r)
	if d == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("only a paired tablet can rename itself", "not_a_device"))
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > 40 {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("name must be 1-40 characters", "name_required"))
		return
	}
	err := s.devices.Rename(r.Context(), d.ID, name)
	if errors.Is(err, device.ErrNotFound) {
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("pair this device first (or log in)", "device_required"))
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"id": d.ID, "name": name})
}

// --- POST /api/device/report-token -----------------------------------------

// mainErrCode maps main's answer to a proxied call onto the agent's error
// contract: 401 not_linked (the wipe hook also fires), 404 main_unsupported
// (main predates the route), 5xx main_unreachable; other 4xx keep their status.
func (s *Server) mainFailure(w http.ResponseWriter, status int, body []byte) {
	switch {
	case status == http.StatusUnauthorized:
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi isn't linked to a store.", "not_linked"))
	case status == http.StatusNotFound:
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("The server doesn't support this yet.", "main_unsupported"))
	case status >= 500 || status == 0:
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("Can't reach the server.", "main_unreachable"))
	default:
		var e struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Code == "" {
			e.Code = "main_rejected"
		}
		if e.Error == "" {
			e.Error = "The server refused the request."
		}
		s.writeJSON(w, status, errCodeMap(e.Error, e.Code))
	}
}

// POST /api/device/report-token — mints (rotates) this tablet's report token
// on main so it can send diagnostics straight to main when the Pi is the
// problem. The agent relays it and never stores it.
func (s *Server) handleReportToken(w http.ResponseWriter, r *http.Request) {
	d := requestDevice(r)
	if d == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("only a paired tablet has a report token", "not_a_device"))
		return
	}
	key, err := s.settings.GetStoreAPIKey(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if key == "" {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi isn't linked to a store.", "not_linked"))
		return
	}
	body, _ := json.Marshal(map[string]string{"tablet_id": d.ID, "tablet_name": d.Name})
	status, resp, err := s.mainClient.Post(r.Context(), key, "/store/v1/agent/tablet-tokens", body)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("Can't reach the server.", "main_unreachable"))
		return
	}
	if status != http.StatusCreated && status != http.StatusOK {
		s.mainFailure(w, status, resp)
		return
	}
	var minted struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(resp, &minted) != nil || minted.Token == "" {
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("The server sent no token.", "main_unreachable"))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"token": minted.Token, "main_url": s.mainClient.BaseURL()})
}

// dropTabletToken revokes a tablet's report token on main in the background
// (best effort: main also caps tokens per location) after the tablet is
// unpaired or revoked here.
func (s *Server) dropTabletToken(id string) {
	if s.mainClient == nil || s.settings == nil || id == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		key, err := s.settings.GetStoreAPIKey(ctx)
		if err != nil || key == "" {
			return
		}
		status, _, err := s.mainClient.Delete(ctx, key, "/store/v1/agent/tablet-tokens/"+id)
		if err != nil || (status >= 300 && status != http.StatusNotFound) {
			s.logger.Debug().Err(err).Int("status", status).Str("device_id", id).Msg("revoking the tablet's report token on main failed")
		}
	}()
}

// --- POST /api/diag/report -------------------------------------------------

// POST /api/diag/report — a tablet's diagnostics report (v1 JSON, <=256 KB),
// stamped with the tablet's identity (from its device key, never the body) and
// a fresh Pi health snapshot, relayed to main as RPC diag.report.
func (s *Server) handleDiagReport(w http.ResponseWriter, r *http.Request) {
	d := requestDevice(r)
	if d == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("only a paired tablet can send a report", "not_a_device"))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDiagReport))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.writeJSON(w, http.StatusRequestEntityTooLarge, errCodeMap("The report is too large.", "too_large"))
			return
		}
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") || !json.Valid(raw) {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("The report must be a JSON object.", "bad_request"))
		return
	}
	key, err := s.settings.GetStoreAPIKey(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if key == "" || s.diag.RPC == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi isn't linked to a store.", "not_linked"))
		return
	}
	pi := map[string]any{"version": s.version}
	if s.diag.Health != nil {
		pi["health"] = s.diag.Health.Snapshot(r.Context())
	}
	payload := map[string]any{"tablet_id": d.ID, "tablet_name": d.Name, "report": json.RawMessage(raw), "pi": pi}

	ctx, cancel := context.WithTimeout(r.Context(), diagReportWait)
	defer cancel()
	status, reply, err := s.diag.RPC.Send(ctx, agentrpc.MsgDiagReport, payload)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("Can't reach the server.", "main_unreachable"))
		return
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
	case http.StatusAccepted: // main took the envelope but has no diag.report handler
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("The server doesn't accept reports yet.", "main_unsupported"))
		return
	case http.StatusBadRequest:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(reply, &e)
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("The server rejected the report: "+e.Error, "bad_report"))
		return
	default:
		s.mainFailure(w, status, reply)
		return
	}
	var res struct {
		ID          string `json:"id"`
		Stored      *bool  `json:"stored"`
		Duplicate   *bool  `json:"duplicate"`
		RateLimited bool   `json:"rate_limited"`
		RetryAfter  int    `json:"retry_after"`
	}
	_ = json.Unmarshal(reply, &res)
	if res.RateLimited {
		body := map[string]any{"error": "Too many reports — try again later.", "code": "rate_limited", "retry_after": res.RetryAfter}
		s.writeJSON(w, http.StatusTooManyRequests, body)
		return
	}
	out := map[string]any{"ok": true}
	if res.ID != "" {
		out["id"] = res.ID
	}
	if res.Stored != nil {
		out["stored"] = *res.Stored
	}
	if res.Duplicate != nil {
		out["duplicate"] = *res.Duplicate
	}
	s.writeJSON(w, http.StatusAccepted, out)
}
