package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	phoneApp "quiccpos/agent/internal/application/phonepay"
	"quiccpos/agent/internal/domain/device"
	pp "quiccpos/agent/internal/domain/phonepay"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/transport/phonepage"

	"github.com/google/uuid"
)

// "Send to phone" (contract §2.10z): the cashier phone's page (/pay, LAN,
// served here), its routes (/api/phone/*, the phone's key in the quicc_phone
// cookie or X-Agent-Device-Key) and the terminal's (/api/terminal/phones*,
// …/phone-charges*).

// phoneCookie carries a paired phone's key.
const phoneCookie = "quicc_phone"

// phoneCookieMaxAge: Chrome keeps a cookie at most 400 days.
const phoneCookieMaxAge = 400 * 24 * 60 * 60

// SetPhonePay wires Send to phone.
func (s *Server) SetPhonePay(p *phoneApp.Service) { s.phonePay = p }

// routes is the whole table: the base routes + Send to phone's.
func (s *Server) routes() []routeDef { return append(s.baseRoutes(), s.phoneRoutes()...) }

func (s *Server) phoneRoutes() []routeDef {
	return []routeDef{
		{"GET", "/pay", "none", "Send to phone (2.10z): the cashier phone's page (pair, wait, CHARGE in Square's app)", s.handlePhonePage("pay.html")},
		{"GET", "/pay/back", "none", "Send to phone: where Square's answer lands (via the HTTPS bounce page); posts it and goes back to /pay", s.handlePhonePage("back.html")},
		{"GET", "/pay/fake-square", "none", "Send to phone: the fake Square POS (dev / staging Pi only; 404 on prod) ?amount&note&meta&cb", s.handleFakeSquare},
		{"POST", "/api/phone/pair", "none", "Pair a cashier phone {code (a phone code from the Pi page or a terminal) | username + password (the manager login), name} -> 201 {device_id, name, kind: phone} + Set-Cookie quicc_phone | 400 | 401 invalid_code/bad_login | 403 kind_not_allowed | 429", s.handlePhonePair},
		{"GET", "/api/phone/me", "phone", "This phone: {phone, store, square: {mode square|fake|off, app_id, callback_url, location_id, reason?}, charges: [Charge], timeout_seconds}", s.handlePhoneMe},
		{"GET", "/api/phone/events", "phone", "SSE for this phone: hello {charges}, phone.charge {charge}; ping every 15 s", s.handlePhoneEvents},
		{"POST", "/api/phone/charges/{id}/open", "phone", "CHARGE pressed: pending -> in_app -> {charge} | 404 | 409 charge_final", s.handlePhoneOpen},
		{"POST", "/api/phone/charges/{id}/result", "phone", "Square's answer {status ok|error|cancelled|went_through, transaction_id?, client_transaction_id?, error_code?} -> {charge, duplicate} (ok = recorded as a square_app payment) | 400 | 404 | 409 charge_final", s.handlePhoneResult},
		{"POST", "/api/phone/charges/{id}/cancel", "phone", "The phone calls a charge off -> {charge} | 404 | 409 charge_final", s.handlePhoneCancel},
		{"POST", "/api/phone/unpair", "phone", "Unpair this phone (revoke its key, clear the cookie) -> 204", s.handlePhoneUnpair},
		{"GET", "/api/terminal/phones", "terminal", "Send to phone: the paired phones {phones: [{id, name, online, last_seen_at?, busy_charge_id?}], square: {mode, reason?}, timeout_seconds}", s.handleTerminalPhones},
		{"POST", "/api/terminal/phones/code", "terminal", "Pair a phone: a 6-digit code for kind phone {manager_pin?, approvals?} -> {code, expires_at} | 403 manager_required {action: pair_phone} | 429", s.handleTerminalPhoneCode},
		{"POST", "/api/terminal/orders/{id}/phone-charges", "terminal", "Send a card charge to a phone {client_key, phone_id, amount_cents, tip_cents?} -> 201 {charge, duplicate: false} | 200 duplicate | 400 | 401 login_required | 404 not_found/unknown_phone | 409 over_balance/not_payable/phone_busy/square_off/key_reused (SSE terminal.phone_charge)", s.handleTerminalPhoneCharge},
		{"GET", "/api/terminal/orders/{id}/phone-charges", "terminal", "An order's phone charges, newest first {charges}", s.handleTerminalOrderPhoneCharges},
		{"GET", "/api/terminal/phone-charges/{id}", "terminal", "One phone charge {charge} | 404", s.handleTerminalGetPhoneCharge},
		{"POST", "/api/terminal/phone-charges/{id}/cancel", "terminal", "Cancel a phone charge -> {charge} | 404 | 409 charge_final", s.handleTerminalCancelPhoneCharge},
		{"POST", "/api/terminal/phone-charges/{id}/record", "terminal", "Record it: find the charge in Square (square.pos_lookup) and record it -> {charge} | 404 | 409 not_found_in_square/square_unavailable {reason}", s.handleTerminalRecordPhoneCharge},
		{"GET", "/api/phone-charges", "session", "The Pi page: the newest phone charges ?limit=N (<= 500)", s.handlePiPhoneCharges},
	}
}

// phone gates /api/phone/*: a paired phone's key (cookie or header) only.
func (s *Server) phone(h http.HandlerFunc) http.HandlerFunc {
	return s.withCORS(func(w http.ResponseWriter, r *http.Request) {
		if s.phonePay == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("Send to phone isn't running", "phone_pay_unavailable"))
			return
		}
		key := r.Header.Get(deviceKeyHeader)
		if key == "" {
			if c, err := r.Cookie(phoneCookie); err == nil {
				key = c.Value
			}
		}
		if key == "" || s.devices == nil {
			s.writeJSON(w, http.StatusUnauthorized, errCodeMap("pair this phone first", "device_required"))
			return
		}
		d, err := s.devices.FindActiveByHash(r.Context(), hashDeviceKey(key))
		if errors.Is(err, device.ErrNotFound) {
			s.writeJSON(w, http.StatusUnauthorized, errCodeMap("this phone isn't paired (any more): pair it again", "device_required"))
			return
		}
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, err)
			return
		}
		if d.Kind != device.KindPhone {
			s.writeJSON(w, http.StatusForbidden, errCodeMap("only a paired cashier phone can use this route", "phone_required"))
			return
		}
		s.touchDevice(d.ID)
		h(w, r.WithContext(context.WithValue(r.Context(), deviceCtxKey{}, d)))
	})
}

func phoneOf(r *http.Request) *device.Device {
	d, _ := r.Context().Value(deviceCtxKey{}).(*device.Device)
	return d
}

// phoneError writes a Send to phone error.
func (s *Server) phoneError(w http.ResponseWriter, r *http.Request, err error) {
	var ref *phoneApp.Refusal
	if errors.As(err, &ref) {
		body := map[string]any{"error": ref.Message, "code": ref.Code}
		for k, v := range ref.Extra {
			body[k] = v
		}
		s.writeJSON(w, ref.Status, body)
		return
	}
	s.terminalError(w, r, err)
}

// ---- the pages ---------------------------------------------------------------

func (s *Server) handlePhonePage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := fs.ReadFile(phonepage.FS, name)
		if err != nil {
			http.Error(w, "page unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write(body)
	}
}

// GET /pay/fake-square: Square's screen in our words (dev / staging only).
// Approve makes the transaction in main's fake Square when main is fake.
func (s *Server) handleFakeSquare(w http.ResponseWriter, r *http.Request) {
	if s.phonePay == nil || !s.phonePay.FakeAllowed() {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if act := q.Get("do"); act != "" { // the page's buttons come back here
		cb := q.Get("cb")
		if !strings.HasPrefix(cb, "/pay/back") { // only ever our own page
			cb = "/pay/back"
		}
		out := map[string]string{"com.squareup.pos.REQUEST_METADATA": q.Get("meta")}
		switch act {
		case "approve":
			amount, _ := strconv.ParseInt(q.Get("amount"), 10, 64)
			tx, client, _ := s.phonePay.FakeCharge(r.Context(), amount, q.Get("note"))
			out["com.squareup.pos.SERVER_TRANSACTION_ID"] = tx
			out["com.squareup.pos.CLIENT_TRANSACTION_ID"] = client
		case "decline":
			out["com.squareup.pos.ERROR_CODE"] = "com.squareup.pos.ERROR_NO_NETWORK"
			out["com.squareup.pos.ERROR_DESCRIPTION"] = "fake decline"
		default:
			out["com.squareup.pos.ERROR_CODE"] = "com.squareup.pos.ERROR_TRANSACTION_CANCELED"
		}
		v := make([]string, 0, len(out))
		for k, val := range out {
			v = append(v, k+"="+url.QueryEscape(val))
		}
		http.Redirect(w, r, cb+"?"+strings.Join(v, "&"), http.StatusSeeOther)
		return
	}
	s.handlePhonePage("fake.html")(w, r)
}

// ---- pairing -------------------------------------------------------------------

// POST /api/phone/pair {code | username + password, name} (open).
func (s *Server) handlePhonePair(w http.ResponseWriter, r *http.Request) {
	if s.phonePay == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("Send to phone isn't running", "phone_pay_unavailable"))
		return
	}
	var body struct {
		Code     string `json:"code"`
		Username string `json:"username"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > 40 {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("Give this phone a name (1-40 characters), e.g. Ali's phone.", "name_required"))
		return
	}
	how := "code"
	switch {
	case strings.TrimSpace(body.Code) != "":
		ok, wrongKind := s.pairing.consumeKind(strings.TrimSpace(body.Code), device.KindPhone, time.Now())
		if wrongKind {
			s.writeJSON(w, http.StatusForbidden, errCodeMap("That code pairs another kind of device; get a phone code from the terminal or the Pi page. It is used up.", "kind_not_allowed"))
			return
		}
		if !ok {
			s.writeJSON(w, http.StatusUnauthorized, errCodeMap("Wrong or expired code: get a new one.", "invalid_code"))
			return
		}
	case strings.TrimSpace(body.Username) != "" && body.Password != "":
		if !s.managerLoginOK(w, r, body.Username, body.Password) {
			return
		}
		how = "manager_login"
	default:
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("Type the code from the terminal, or the store manager login.", "bad_request"))
		return
	}
	key, err := newDeviceKey()
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	id := uuid.NewString()
	if err := s.devices.Create(r.Context(), id, name, device.KindPhone, hashDeviceKey(key), key[:len(deviceKeyPrefix)+6]); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.logger.Info().Ctx(r.Context()).Str("device_id", id).Str("name", name).Str("kind", device.KindPhone).Str("with", how).Msg("Device paired")
	s.journalPaired(r.Context(), id, name, how)
	s.phonePay.PhonesChanged()
	http.SetCookie(w, &http.Cookie{Name: phoneCookie, Value: key, Path: "/", MaxAge: phoneCookieMaxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	s.writeJSON(w, http.StatusCreated, map[string]any{"device_id": id, "name": name, "kind": device.KindPhone})
}

// POST /api/phone/unpair (phone).
func (s *Server) handlePhoneUnpair(w http.ResponseWriter, r *http.Request) {
	d := phoneOf(r)
	if err := s.devices.Revoke(r.Context(), d.ID, time.Now()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.phonePay.PhonesChanged()
	http.SetCookie(w, &http.Cookie{Name: phoneCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

// ---- the phone ---------------------------------------------------------------

func (s *Server) handlePhoneMe(w http.ResponseWriter, r *http.Request) {
	d := phoneOf(r)
	charges, err := s.phonePay.PhoneCharges(r.Context(), d.ID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"phone":           map[string]any{"id": d.ID, "name": d.Name},
		"store":           map[string]any{"name": s.storeContext().Location.Name},
		"square":          s.phonePay.Square(r.Context()),
		"charges":         charges,
		"timeout_seconds": s.phonePay.TimeoutSeconds(),
	})
}

// phonePing: the phone page's keepalive.
var phonePing = 15 * time.Second

func (s *Server) handlePhoneEvents(w http.ResponseWriter, r *http.Request) {
	d := phoneOf(r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, stop := s.phonePay.Subscribe(d.ID)
	defer stop()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	write := func(event string, v any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(eventsWriteTimeout))
		var err error
		if event == "" {
			_, err = fmt.Fprint(w, ": ping\n\n")
		} else {
			b, _ := json.Marshal(v)
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		}
		if err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	charges, err := s.phonePay.PhoneCharges(r.Context(), d.ID)
	if err != nil || !write("hello", map[string]any{"charges": charges}) {
		return
	}
	t := time.NewTicker(phonePing)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case c := <-ch:
			if !write("phone.charge", map[string]any{"charge": c}) {
				return
			}
		case <-t.C:
			if !write("", nil) {
				return
			}
		}
	}
}

func (s *Server) handlePhoneOpen(w http.ResponseWriter, r *http.Request) {
	c, err := s.phonePay.Open(r.Context(), phoneOf(r).ID, r.PathValue("id"))
	if err != nil {
		s.phoneError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"charge": c})
}

func (s *Server) handlePhoneResult(w http.ResponseWriter, r *http.Request) {
	var in pp.Result
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	c, dup, err := s.phonePay.Result(r.Context(), phoneOf(r).ID, r.PathValue("id"), in)
	if err != nil {
		s.phoneError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"charge": c, "duplicate": dup})
}

func (s *Server) handlePhoneCancel(w http.ResponseWriter, r *http.Request) {
	c, err := s.phonePay.Cancel(r.Context(), phoneOf(r).ID, r.PathValue("id"), "phone")
	if err != nil {
		s.phoneError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"charge": c})
}

// ---- the terminal ------------------------------------------------------------

func (s *Server) phonePayReady(w http.ResponseWriter) bool {
	if s.phonePay == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("Send to phone isn't running", "phone_pay_unavailable"))
		return false
	}
	return true
}

func (s *Server) handleTerminalPhones(w http.ResponseWriter, r *http.Request) {
	if !s.phonePayReady(w) {
		return
	}
	phones, err := s.phonePay.Phones(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	sq := s.phonePay.Square(r.Context())
	square := map[string]any{"mode": sq.Mode}
	if sq.Reason != "" {
		square["reason"] = sq.Reason
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"phones": phones, "square": square, "timeout_seconds": s.phonePay.TimeoutSeconds()})
}

func (s *Server) handleTerminalPhoneCode(w http.ResponseWriter, r *http.Request) {
	if !s.phonePayReady(w) {
		return
	}
	var body struct {
		ManagerPIN string   `json:"manager_pin"`
		Approvals  []string `json:"approvals"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
			return
		}
	}
	a := actor(r)
	if _, err := s.phonePay.PhoneCodeAllowed(r.Context(), a, body.ManagerPIN, body.Approvals); err != nil {
		s.phoneError(w, r, err)
		return
	}
	code, exp, err := s.pairing.issueFor(a.DeviceID, device.KindPhone, time.Now())
	var many *errTooManyCodes
	if errors.As(err, &many) {
		w.Header().Set("Retry-After", strconv.Itoa(int(many.wait.Seconds())+1))
		s.writeJSON(w, http.StatusTooManyRequests, errCodeMap(err.Error(), "too_many_codes"))
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if s.terminalSvc != nil {
		s.terminalSvc.RecordPi(r.Context(), a.DeviceID, terminal.ActionPairingCode, map[string]any{"device_id": a.DeviceID, "device_name": a.DeviceName, "kind": device.KindPhone})
	}
	// url: the page the phone opens, at the address this terminal reached the Pi by.
	s.writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires_at": exp.UTC(), "kind": device.KindPhone,
		"url": "http://" + r.Host + "/pay"})
}

func (s *Server) handleTerminalPhoneCharge(w http.ResponseWriter, r *http.Request) {
	if !s.phonePayReady(w) {
		return
	}
	var in phoneApp.CreateIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	c, dup, err := s.phonePay.Create(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		s.phoneError(w, r, err)
		return
	}
	status := http.StatusCreated
	if dup {
		status = http.StatusOK
	}
	s.writeJSON(w, status, map[string]any{"charge": c, "duplicate": dup})
}

func (s *Server) handleTerminalOrderPhoneCharges(w http.ResponseWriter, r *http.Request) {
	if !s.phonePayReady(w) {
		return
	}
	list, err := s.phonePay.ForOrder(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"charges": list})
}

func (s *Server) handleTerminalGetPhoneCharge(w http.ResponseWriter, r *http.Request) {
	if !s.phonePayReady(w) {
		return
	}
	c, err := s.phonePay.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.phoneError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"charge": c})
}

func (s *Server) handleTerminalCancelPhoneCharge(w http.ResponseWriter, r *http.Request) {
	if !s.phonePayReady(w) {
		return
	}
	c, err := s.phonePay.Cancel(r.Context(), "", r.PathValue("id"), "terminal")
	if err != nil {
		s.phoneError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"charge": c})
}

func (s *Server) handleTerminalRecordPhoneCharge(w http.ResponseWriter, r *http.Request) {
	if !s.phonePayReady(w) {
		return
	}
	c, err := s.phonePay.Record(r.Context(), r.PathValue("id"))
	if err != nil {
		s.phoneError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"charge": c})
}

func (s *Server) handlePiPhoneCharges(w http.ResponseWriter, r *http.Request) {
	if !s.phonePayReady(w) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.phonePay.Recent(r.Context(), limit)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"charges": list})
}
