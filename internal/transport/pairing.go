package transport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// Tablet pairing + device-key auth.
//
// The web UI (session) or an already-paired tablet asks for a 6-digit code
// (POST /api/pairing/code, 5-min TTL, one live code at a time, burned after 5
// wrong guesses); the tablet
// sends it with a name to POST /api/pair and gets a device key back once. The
// key (qagt_…) goes in X-Agent-Device-Key on every floor request; only its
// sha256 is stored. Devices are listed + revoked in the web UI's Settings.
//
// A device paired with kind "integration" (the store hub, 2.1.0) is not a
// tablet: its key opens /api/integration/* (which tablet keys and web
// sessions can't) plus GET /api/floor, /api/events, /api/drivers and
// /api/settings/*, nothing else (403 integration_only).

const (
	deviceKeyHeader  = "X-Agent-Device-Key"
	deviceKeyPrefix  = "qagt_"
	pairingTTL       = 5 * time.Minute
	pairingMaxMisses = 5
	touchEvery       = time.Minute
)

// Codes a device may mint (X-1, contract 2.10s §8): at most
// pairingCodesPerDevice per pairingCodesWindow per device ("web" = the
// session).
const (
	pairingCodesPerDevice = 5
	pairingCodesWindow    = 15 * time.Minute
)

type pairingCodes struct {
	mu      sync.Mutex
	code    string
	expires time.Time
	misses  int
	// kind: what the live code pairs ("" = any kind: a web-session code
	// minted without one). A tablet's code pairs tablets only.
	kind   string
	minted map[string][]time.Time // device id -> codes minted in the window
}

// errTooManyCodes: the device minted its pairingCodesPerDevice for the window.
type errTooManyCodes struct{ wait time.Duration }

func (e *errTooManyCodes) Error() string { return "too many pairing codes - wait a few minutes" }

func (p *pairingCodes) issue(now time.Time) (string, time.Time, error) {
	return p.issueFor("", "", now)
}

// issueFor mints the live code for device, bound to kind ("" = any).
func (p *pairingCodes) issueFor(device, kind string, now time.Time) (string, time.Time, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", time.Time{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if device != "" {
		if p.minted == nil {
			p.minted = map[string][]time.Time{}
		}
		var keep []time.Time
		for _, t := range p.minted[device] {
			if now.Sub(t) < pairingCodesWindow {
				keep = append(keep, t)
			}
		}
		if len(keep) >= pairingCodesPerDevice {
			p.minted[device] = keep
			return "", time.Time{}, &errTooManyCodes{wait: keep[0].Add(pairingCodesWindow).Sub(now)}
		}
		p.minted[device] = append(keep, now)
	}
	p.code = fmt.Sprintf("%06d", n.Int64())
	p.expires = now.Add(pairingTTL)
	p.misses = 0
	p.kind = kind
	return p.code, p.expires, nil
}

// consume reports whether code is the live one; success or too many misses
// burns it.
func (p *pairingCodes) consume(code string, now time.Time) bool {
	ok, _ := p.consumeKind(code, "", now)
	return ok
}

// consumeKind: the live code for a device of kind (kind "" = don't check).
// A right code bound to another kind is burned (wrongKind true).
func (p *pairingCodes) consumeKind(code, kind string, now time.Time) (ok, wrongKind bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code == "" || now.After(p.expires) {
		p.code = ""
		return false, false
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(p.code)) == 1 {
		bound := p.kind
		p.code, p.kind = "", ""
		if kind != "" && bound != "" && bound != kind {
			return false, true
		}
		return true, false
	}
	p.misses++
	if p.misses >= pairingMaxMisses {
		p.code = ""
	}
	return false, false
}

func hashDeviceKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func newDeviceKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return deviceKeyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

type deviceCtxKey struct{}

// deviceFromRequest resolves X-Agent-Device-Key to an active paired device.
// err is set only for a lookup failure (not for an unknown/revoked key), so a
// DB hiccup isn't reported as "re-pair" to the tablet.
func (s *Server) deviceFromRequest(r *http.Request) (*device.Device, bool, error) {
	key := r.Header.Get(deviceKeyHeader)
	if key == "" || s.devices == nil {
		return nil, false, nil
	}
	d, err := s.devices.FindActiveByHash(r.Context(), hashDeviceKey(key))
	if errors.Is(err, device.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	s.touchDevice(d.ID)
	if v := r.Header.Get(quibaseVersionHeader); v != "" {
		s.noteAppVersion(d, v)
	} else if v := r.Header.Get(terminalVersionHeader); v != "" && d.Kind == device.KindTerminal {
		s.noteAppVersion(d, v) // Centurion's build (contract 2.10y)
	}
	return d, true, nil
}

// touchDevice stamps last_seen_at at most once a minute per device.
func (s *Server) touchDevice(id string) {
	now := time.Now()
	s.touchMu.Lock()
	last := s.touched[id]
	if now.Sub(last) < touchEvery {
		s.touchMu.Unlock()
		return
	}
	s.touched[id] = now
	s.touchMu.Unlock()
	if err := s.devices.Touch(context.Background(), id, now); err != nil {
		s.logger.Warn().Err(err).Str("device_id", id).Msg("touch paired device failed")
	}
}

// floor gates the floor routes: a paired tablet (device key) OR a logged-in
// web-UI session (the backup accept screen). An integration key gets only
// the read-only routes integrationMayRead allows.
func (s *Server) floor(h http.HandlerFunc) http.HandlerFunc {
	return s.withCORS(func(w http.ResponseWriter, r *http.Request) {
		if s.session != nil && s.session.valid(r) {
			h(w, r)
			return
		}
		d, ok, err := s.deviceFromRequest(r)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, err)
			return
		}
		if ok {
			if d.Kind == device.KindIntegration && !integrationMayRead(r) {
				s.writeJSON(w, http.StatusForbidden, errCodeMap("an integration key can't use this route", "integration_only"))
				return
			}
			if d.Kind == device.KindPhone { // a cashier phone opens /api/phone/* only (2.10z)
				s.writeJSON(w, http.StatusForbidden, errCodeMap("a phone key opens /api/phone/* only", "phone_only"))
				return
			}
			if d.Kind == device.KindTerminal && !terminalMayUse(r) {
				s.writeJSON(w, http.StatusForbidden, errCodeMap("a terminal key opens /api/terminal/*, /api/events and GET /api/printers only", "terminal_only"))
				return
			}
			h(w, r.WithContext(context.WithValue(r.Context(), deviceCtxKey{}, d)))
			return
		}
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("pair this device first (or log in)", "device_required"))
	})
}

// integrationMayRead: the floor routes an integration key may call — GET
// /api/floor, /api/events, /api/drivers and /api/settings/*.
func integrationMayRead(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch p := r.URL.Path; {
	case p == "/api/floor", p == "/api/events", p == "/api/drivers":
		return true
	case strings.HasPrefix(p, "/api/settings/"):
		return true
	}
	return false
}

// terminalMayUse: the floor routes a POS terminal's key may call besides
// /api/terminal/* - the event stream, the floor summary, renaming or
// unpairing itself, and the setup wizard's diagnostics (2.10y).
func terminalMayUse(r *http.Request) bool {
	switch p := r.URL.Path; {
	case r.Method == http.MethodGet && (p == "/api/events" || p == "/api/floor" || p == "/api/printers"):
		return true // GET /api/printers: the terminal's printer alert (read only)
	case r.Method == http.MethodPatch && p == "/api/device/self", r.Method == http.MethodPost && p == "/api/device/unpair":
		return true
	// The setup wizard (contract 2.10y): Pi health, network printers, test
	// slips with no failover, its setup report.
	case r.Method == http.MethodGet && (p == "/api/diag" || p == "/api/printers/found"):
		return true
	case r.Method == http.MethodPost && (p == "/api/printers/test" || p == "/api/diag/report"):
		return true
	// The width test of a kitchen printer (contract 2.3c "Paper width").
	case r.Method == http.MethodPost && (p == "/api/printers/width-test" || p == "/api/printers/width"):
		return true
	}
	return false
}

// terminal gates /api/terminal/*: a paired POS terminal's key, or the web-UI
// session (tests, the Pi web page); a tablet or integration key gets 403.
func (s *Server) terminal(h http.HandlerFunc) http.HandlerFunc {
	return s.withCORS(func(w http.ResponseWriter, r *http.Request) {
		d, ok, err := s.deviceFromRequest(r)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, err)
			return
		}
		switch {
		case ok && d.Kind == device.KindTerminal:
			h(w, r.WithContext(context.WithValue(r.Context(), deviceCtxKey{}, d)))
		case ok:
			s.writeJSON(w, http.StatusForbidden, errCodeMap("only a paired POS terminal can use this route", "terminal_required"))
		case s.session != nil && s.session.valid(r):
			h(w, r)
		default:
			s.writeJSON(w, http.StatusUnauthorized, errCodeMap("pair this terminal first (or log in)", "device_required"))
		}
	})
}

// integration gates /api/integration/*: an integration device's key only
// (a tablet key or a web session gets 403).
func (s *Server) integration(h http.HandlerFunc) http.HandlerFunc {
	return s.withCORS(func(w http.ResponseWriter, r *http.Request) {
		d, ok, err := s.deviceFromRequest(r)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, err)
			return
		}
		switch {
		case ok && d.Kind == device.KindIntegration:
			h(w, r.WithContext(context.WithValue(r.Context(), deviceCtxKey{}, d)))
		case ok || (s.session != nil && s.session.valid(r)):
			s.writeJSON(w, http.StatusForbidden, errCodeMap("only the store hub's integration key can use this route", "integration_required"))
		default:
			s.writeJSON(w, http.StatusUnauthorized, errCodeMap("pair the integration first", "device_required"))
		}
	})
}

// POST /api/pairing/code {kind?} (session or a paired tablet's key, so a
// tablet can pair another one) — show a fresh 6-digit code. X-1 (contract
// 2.10s §8): a tablet's code pairs tablets only (another kind asked = 403
// kind_not_allowed); the session's pairs the kind asked for (none = any).
// At most 5 codes per device per 15 min (429 too_many_codes); journaled
// pairing.code.
func (s *Server) handlePairingCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"`
		// Username / Password (contract 2.10y): a tablet may show a terminal
		// code when the store manager login is typed with it.
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
			return
		}
	}
	kind := strings.TrimSpace(body.Kind)
	switch kind {
	case "", device.KindTablet, device.KindIntegration, device.KindTerminal, device.KindPhone: // phone: 2.10z
	default:
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("kind must be tablet, integration, terminal or phone", "bad_request"))
		return
	}
	minter, name, via := "web", "Pi web page", ""
	if d, ok := r.Context().Value(deviceCtxKey{}).(*device.Device); ok {
		minter, name = d.ID, d.Name
		if d.Kind != device.KindTablet {
			s.writeJSON(w, http.StatusForbidden, errCodeMap("only a tablet or the Pi web page can show a pairing code", "kind_not_allowed"))
			return
		}
		switch {
		case kind == device.KindTerminal && strings.TrimSpace(body.Username) != "" && body.Password != "":
			// Quibase's "Pair a counter terminal" (2.10y): the manager login.
			if !s.managerLoginOK(w, r, body.Username, body.Password) {
				return
			}
			via = "manager_login"
		case kind != "" && kind != device.KindTablet:
			s.writeJSON(w, http.StatusForbidden, errCodeMap("a tablet's code pairs tablets only - a terminal code needs the store manager login; the hub pairs from the Pi web page", "kind_not_allowed"))
			return
		default:
			kind = device.KindTablet
		}
	}
	code, exp, err := s.pairing.issueFor(minter, kind, time.Now())
	var many *errTooManyCodes
	if errors.As(err, &many) {
		w.Header().Set("Retry-After", strconv.Itoa(int(many.wait.Seconds())+1))
		s.writeJSON(w, http.StatusTooManyRequests, errCodeMap(err.Error(), "too_many_codes"))
		return
	}
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, errCodeMap("could not generate a code", "internal"))
		return
	}
	shown := kind
	if shown == "" {
		shown = "any"
	}
	s.logger.Info().Ctx(r.Context()).Str("device_id", minter).Str("kind", shown).Msg("Pairing code shown")
	if s.terminalSvc != nil {
		d := map[string]any{"device_id": minter, "device_name": name, "kind": shown}
		if via != "" {
			d["via"] = via
		}
		s.terminalSvc.RecordPi(r.Context(), minter, terminal.ActionPairingCode, d)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires_at": exp.UTC(), "kind": shown})
}

// POST /api/pair {code, name, kind?} (open) — exchange a code for a device
// key. kind: tablet (default) | integration (the store hub) | terminal (a
// POS terminal, 0d).
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > 40 {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("name must be 1-40 characters", "name_required"))
		return
	}
	kind := strings.TrimSpace(req.Kind)
	switch kind {
	case "":
		kind = device.KindTablet
	case device.KindTablet, device.KindIntegration, device.KindTerminal:
	default:
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("kind must be tablet, integration or terminal", "bad_request"))
		return
	}
	ok, wrongKind := s.pairing.consumeKind(strings.TrimSpace(req.Code), kind, time.Now())
	if wrongKind {
		s.logger.Warn().Ctx(r.Context()).Str("kind", kind).Msg("Pairing refused: the code was minted for another kind (burned)")
		s.writeJSON(w, http.StatusForbidden, errCodeMap("this code pairs another kind of device (a tablet's code pairs tablets only); it is used up", "kind_not_allowed"))
		return
	}
	if !ok {
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("wrong or expired pairing code", "invalid_code"))
		return
	}
	s.pairDevice(w, r, name, kind, "code")
}

// pairDevice creates a paired device and answers 201 with its key — the
// tail of POST /api/pair and POST /api/pair/manager.
func (s *Server) pairDevice(w http.ResponseWriter, r *http.Request, name, kind, how string) {
	key, err := newDeviceKey()
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	id := uuid.NewString()
	if err := s.devices.Create(r.Context(), id, name, kind, hashDeviceKey(key), key[:len(deviceKeyPrefix)+6]); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.logger.Info().Ctx(r.Context()).Str("device_id", id).Str("name", name).Str("kind", kind).Str("with", how).Msg("Device paired")
	s.journalPaired(r.Context(), id, name, how)
	if kind == device.KindIntegration && s.broker != nil {
		s.broker.PublishFloorChanged() // the web UI shows the POS switches now
	}
	// device_key is the same key under the name the integration contract uses.
	s.writeJSON(w, http.StatusCreated, map[string]any{"device_id": id, "name": name, "kind": kind, "key": key, "device_key": key})
}

// POST /api/pair/manager {username, password, tablet_name} (open, 2.4.2) — a
// tablet joins a linked Pi with the store manager login, for when there is no
// working tablet to get a code from (the only one was reinstalled). Same
// login, hash refresh and per-IP throttle as the web UI's /api/login; the
// tablet is created like POST /api/pair.
func (s *Server) handlePairManager(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key, err := s.settings.GetStoreAPIKey(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if key == "" {
		s.writeJSON(w, http.StatusConflict, errCodeMap("This Pi isn't set up yet — set it up with the manager login first.", "not_linked"))
		return
	}
	li, err := s.settings.GetLinkInfo(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	hash, _, err := s.settings.GetUIPassword(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if li.ManagerUsername == "" || hash == "" {
		s.writeJSON(w, http.StatusConflict, errCodeMap("This Pi doesn't use the manager login — get a pairing code on its web page.", "manager_login_unavailable"))
		return
	}
	var body struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TabletName string `json:"tablet_name"`
		// Kind (X-1): tablet (default), terminal or integration - a terminal
		// or the hub pairs only with a web-session code or this login.
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	name := strings.TrimSpace(body.TabletName)
	if name == "" || utf8.RuneCountInString(name) > 40 {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("name must be 1-40 characters", "name_required"))
		return
	}
	kind := strings.TrimSpace(body.Kind)
	switch kind {
	case "":
		kind = device.KindTablet
	case device.KindTablet, device.KindIntegration, device.KindTerminal:
	default:
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("kind must be tablet, integration or terminal", "bad_request"))
		return
	}
	if !s.checkManagerLogin(w, r, hash, body.Username, body.Password) {
		return
	}
	s.pairDevice(w, r, name, kind, "manager_login")
}

// checkManagerLogin checks the store manager login against hash under the
// per-IP login throttle (shared with /api/login): false = answered (429 /
// 401 bad_login).
func (s *Server) checkManagerLogin(w http.ResponseWriter, r *http.Request, hash, username, password string) bool {
	ip := clientIP(r)
	release, wait := s.logins.begin(ip, time.Now())
	if wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		s.writeJSON(w, http.StatusTooManyRequests, errCodeMap("Too many wrong passwords — wait a minute and try again.", "too_many_attempts"))
		return false
	}
	defer release()
	if !s.passwordOK(r, hash, username, password) {
		s.logins.fail(ip, time.Now())
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("Wrong manager username or password.", "bad_login"))
		return false
	}
	s.logins.ok(ip)
	return true
}

// managerLoginOK is checkManagerLogin on a linked Pi that uses the manager
// login (409 manager_login_unavailable otherwise).
func (s *Server) managerLoginOK(w http.ResponseWriter, r *http.Request, username, password string) bool {
	li, err := s.settings.GetLinkInfo(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return false
	}
	hash, _, err := s.settings.GetUIPassword(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return false
	}
	if li.ManagerUsername == "" || hash == "" {
		s.writeJSON(w, http.StatusConflict, errCodeMap("This Pi doesn't use the manager login — get a pairing code on its web page.", "manager_login_unavailable"))
		return false
	}
	return s.checkManagerLogin(w, r, hash, username, password)
}

// GET /api/devices (session).
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	ds, err := s.devices.List(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	// The store's call-center device (contract 2.10ab) goes last: the paired
	// devices keep their newest-first order.
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Kind != device.KindCallcenter && ds[j].Kind == device.KindCallcenter })
	setups := s.deviceSetups(r.Context()) // + each device's last setup (2.10y)
	out := make([]deviceView, len(ds))
	for i, d := range ds {
		out[i] = deviceView{Device: d}
		if res, ok := setups[d.ID]; ok {
			out[i].Setup = &res
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}

// POST /api/devices/{id}/revoke (session).
func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if isCallcenterDevice(id) { // the store's call-center device stays (contract 2.10ab)
		s.writeJSON(w, http.StatusConflict, errCodeMap("The call-center device can't be removed: revoke a call-center computer in the store's Staff page.", "callcenter_device"))
		return
	}
	err := s.devices.Revoke(r.Context(), id, time.Now())
	if errors.Is(err, device.ErrNotFound) {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("no such active device", "not_found"))
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.dropTabletToken(id)
	w.WriteHeader(http.StatusNoContent)
}
