package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/safego"
)

// The call center: a remote terminal over main (contract/agent.md §2.10ab).
// main relays each call of the call-center web app as the RPC command
// terminal.relay; the Pi runs it through the SAME /api/terminal/* handlers
// (HandleTerminalRelay builds the request, a recorder takes the answer) as
// a call-center seat with the agent main signed in.
//
// The seat is a context value only (callcenterCtxKey): no header, cookie or
// query can set it, so nothing over plain HTTP ever acts as the call center.

type callcenterCtxKey struct{}

// callcenterActor is the call-center seat a relayed request runs as.
func callcenterActor(r *http.Request) (terminalApp.Actor, bool) {
	a, ok := r.Context().Value(callcenterCtxKey{}).(terminalApp.Actor)
	return a, ok
}

// Relay limits.
const (
	relayDeadline      = 10 * time.Second // a relayed call's handler
	relayDeadlineKeyed = 30 * time.Second // square/keyed-payments (Square answers slowly)
	relayMaxBody       = 256 << 10
	// RelayMaxInFlight: relayed calls running at once (main gets `busy`
	// over that; cmd/main.go registers the command with it).
	RelayMaxInFlight = 8
)

const relayKeyedPath = "square/keyed-payments"

var relaySeatRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// relayIn is the terminal.relay command's payload.
type relayIn struct {
	Device     string            `json:"device"`
	DeviceName string            `json:"device_name"`
	StaffID    string            `json:"staff_id"`
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Query      string            `json:"query"`
	Headers    map[string]string `json:"headers"`
	Body       json.RawMessage   `json:"body"`
}

// RelayReply is terminal.relay's reply: what the handler wrote.
type RelayReply struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

func relayAnswer(status int, body any) RelayReply {
	raw, err := json.Marshal(body)
	if err != nil {
		raw = []byte("null")
	}
	return RelayReply{Status: status, Headers: map[string]string{}, Body: raw}
}

// RelayBusy is the reply when RelayMaxInFlight calls are running (main
// answers the browser 503 store_offline: nothing ran).
func RelayBusy() RelayReply {
	return relayAnswer(http.StatusServiceUnavailable, errCodeMap("The store's Pi is busy: try again.", "busy"))
}

// relayRecorder takes a handler's answer (status, headers, body).
type relayRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *relayRecorder) Header() http.Header { return r.header }
func (r *relayRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *relayRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

// relayHandler is the mux the relay serves from: the route table's
// /api/terminal/* handlers and GET /api/floor, with no auth wrapper (the
// relay is the auth: main's store stream + the seat in the context). Never
// mounted on a listener.
func (s *Server) relayHandler() http.Handler {
	s.relayOnce.Do(func() {
		mux := http.NewServeMux()
		for _, rt := range s.routes() {
			if strings.HasPrefix(rt.Path, "/api/terminal/") || (rt.Method == http.MethodGet && rt.Path == "/api/floor") {
				mux.HandleFunc(rt.Method+" "+rt.Path, rt.Handler)
			}
		}
		s.relayMux = mux
	})
	return s.relayMux
}

// relayOwn is the relay's own refusal of a call, journaled nowhere (main
// audits callcenter.refused).
func relayRefused(what string) RelayReply {
	e := &terminal.NotAllowedForCallcenter{What: what}
	return relayAnswer(http.StatusForbidden, map[string]any{"error": e.Error(), "code": "not_allowed_for_callcenter", "what": what})
}

// HandleTerminalRelay is the RPC command terminal.relay (main -> Pi,
// request / reply): one call of the call-center app, run through the
// terminal handlers. It always answers a RelayReply (the error is nil): a
// refusal is a status in the reply.
func (s *Server) HandleTerminalRelay(ctx context.Context, payload json.RawMessage) (any, error) {
	var in relayIn
	if err := json.Unmarshal(payload, &in); err != nil {
		return relayAnswer(http.StatusBadRequest, errCodeMap("invalid relay payload", "bad_request")), nil
	}
	if s.terminalSvc == nil {
		return relayAnswer(http.StatusServiceUnavailable, errCodeMap("the terminal API isn't running", "terminal_unavailable")), nil
	}
	seat := strings.TrimSpace(in.Device)
	if !relaySeatRe.MatchString(seat) {
		return relayAnswer(http.StatusBadRequest, errCodeMap("device must be 1-64 letters, digits, _ . -", "bad_request")), nil
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	path := strings.TrimPrefix(strings.TrimSpace(in.Path), "/")
	if !terminalApp.CallcenterAllowed(method, path) {
		s.logger.Warn().Ctx(ctx).Str("method", method).Str("path", path).Str("seat", seat).Msg("Call center: route not allowed")
		return relayRefused(terminal.NotAllowedRoute), nil
	}
	body := []byte(in.Body)
	if len(body) == 0 || string(bytes.TrimSpace(body)) == "null" {
		body = nil
	}
	if len(body) > relayMaxBody {
		return relayAnswer(http.StatusRequestEntityTooLarge, errCodeMap("the body is at most 256 KB", "too_large")), nil
	}
	if relayCarriesManager(body) {
		return relayRefused(terminal.NotAllowedManager), nil
	}
	actor := terminalApp.CallcenterActor(seat, in.DeviceName, in.StaffID)
	if err := s.terminalSvc.CallcenterSession(ctx, actor); err != nil {
		return relayAnswer(http.StatusUnauthorized, errCodeMap(terminal.ErrLoginRequired.Error(), "login_required")), nil
	}
	s.ensureCallcenterDevice(ctx)

	deadline := relayDeadline
	if path == relayKeyedPath {
		deadline = relayDeadlineKeyed
	}
	rctx, cancel := context.WithTimeout(context.WithValue(ctx, callcenterCtxKey{}, actor), deadline)
	defer cancel()
	target := "/api/terminal/" + path
	if path == "floor" {
		target = "/api/floor"
	}
	if q := strings.TrimPrefix(strings.TrimSpace(in.Query), "?"); q != "" {
		target += "?" + q
	}
	req, err := http.NewRequestWithContext(rctx, method, "http://pi"+target, bytes.NewReader(body))
	if err != nil {
		return relayAnswer(http.StatusBadRequest, errCodeMap("bad path or query", "bad_request")), nil
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range in.Headers {
		if http.CanonicalHeaderKey(k) == "If-None-Match" { // the only request header a relayed call carries
			req.Header.Set("If-None-Match", v)
		}
	}
	req.RemoteAddr = "callcenter"

	rec := &relayRecorder{header: http.Header{}}
	if perr := safego.Do(rctx, "relay."+method+" "+path, func() { s.relayHandler().ServeHTTP(rec, req) }); perr != nil {
		return relayAnswer(http.StatusInternalServerError, errCodeMap("internal error", "internal")), nil
	}
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if rctx.Err() != nil && rec.status >= http.StatusInternalServerError {
		// The deadline passed under the handler: with a client_key /
		// idempotency_key the same call again is safe (contract 2.10s).
		s.logger.Warn().Ctx(ctx).Str("method", method).Str("path", path).Dur("deadline", deadline).Msg("Call center: relayed call timed out")
		return relayAnswer(http.StatusServiceUnavailable, errCodeMap("The store's Pi didn't finish in time: the same call again is safe.", "timeout")), nil
	}
	out := RelayReply{Status: rec.status, Headers: map[string]string{}, Body: json.RawMessage("null")}
	for _, h := range []string{"ETag", "Retry-After"} {
		if v := rec.header.Get(h); v != "" {
			out.Headers[h] = v
		}
	}
	if b := bytes.TrimSpace(rec.body.Bytes()); len(b) > 0 {
		if json.Valid(b) {
			out.Body = append(json.RawMessage(nil), b...)
		} else {
			out.Body, _ = json.Marshal(string(b))
		}
	}
	return out, nil
}

// relayCarriesManager: the body names a manager (manager_pin / approvals):
// the call center never uses one.
func relayCarriesManager(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return false // not an object: the handler answers 400
	}
	if raw, ok := top["manager_pin"]; ok {
		var pin string
		if json.Unmarshal(raw, &pin) != nil || strings.TrimSpace(pin) != "" {
			return string(bytes.TrimSpace(raw)) != "null"
		}
	}
	if raw, ok := top["approvals"]; ok {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil {
			return string(bytes.TrimSpace(raw)) != "null"
		}
		return len(list) > 0
	}
	return false
}

// --- the device ----------------------------------------------------------------------

type callcenterDevice struct {
	mu sync.Mutex
	ok bool
}

// ensureCallcenterDevice makes the store's one call-center device (kind
// callcenter, id "callcenter", no key) once per process: at start on a
// linked Pi, at link, and before the first relayed call.
func (s *Server) ensureCallcenterDevice(ctx context.Context) {
	if s.devices == nil {
		return
	}
	s.ccDevice.mu.Lock()
	defer s.ccDevice.mu.Unlock()
	if s.ccDevice.ok {
		return
	}
	if err := s.devices.EnsureCallcenter(context.WithoutCancel(ctx)); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("call-center device: create failed (tried again at the next call)")
		return
	}
	s.ccDevice.ok = true
}

// forgetCallcenterDevice: an unlink revoked every device; the next link
// (or relayed call) makes the call-center device again.
func (s *Server) forgetCallcenterDevice() {
	s.ccDevice.mu.Lock()
	s.ccDevice.ok = false
	s.ccDevice.mu.Unlock()
}

// EnsureCallcenterDevice makes the call-center device when this Pi is
// linked to a store (cmd/main.go at start).
func (s *Server) EnsureCallcenterDevice(ctx context.Context) {
	if s.settings == nil {
		return
	}
	if key, err := s.settings.GetStoreAPIKey(ctx); err != nil || key == "" {
		return
	}
	s.ensureCallcenterDevice(ctx)
}

// --- POST /api/terminal/orders/{id}/reprint (every terminal kind) --------------------

// handleTerminalReprint prints the order's customer receipt on the
// location's receipt printer with failover (the tablet's Reprint) ->
// {ok: true, printed_on} | 401 login_required | 404 | 502 nothing_printed |
// 503 printing_unavailable; journal `reprint {number}`.
func (s *Server) handleTerminalReprint(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	res, err := s.terminalSvc.Reprint(r.Context(), actor(r), r.PathValue("id"))
	switch {
	case err == nil:
		s.writeJSON(w, http.StatusOK, res)
	case errors.Is(err, order.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, errCodeMap("no such order", "not_found"))
	case errors.Is(err, orderApp.ErrPrintingUnavailable):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi has no printer to print the receipt on.", "printing_unavailable"))
	case errors.Is(err, orderApp.ErrKitchenNotPrinted):
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("The receipt didn't print: check the store's receipt printer.", "nothing_printed"))
	default:
		s.terminalError(w, r, err)
	}
}

// isCallcenterDevice: the store's virtual call-center device (never
// renamed, revoked or paired).
func isCallcenterDevice(id string) bool { return id == device.CallcenterID }
