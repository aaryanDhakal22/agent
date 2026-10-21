package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/transport/web"

	"golang.org/x/crypto/bcrypt"
)

// protected wraps a handler with CORS + a session-cookie check. Unauthenticated
// requests get 401 so the UI can bounce to the login screen.
func (s *Server) protected(h http.HandlerFunc) http.HandlerFunc {
	return s.withCORS(func(w http.ResponseWriter, r *http.Request) {
		if s.session == nil || !s.session.valid(r) {
			s.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		h(w, r)
	})
}

// --- web UI ---------------------------------------------------------------

// handleUI serves the embedded single-page app. It's the GET catch-all, so it
// also receives unmatched /api/* GETs — those get a clean 404 instead of the
// HTML page.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	body, err := fs.ReadFile(web.FS, "index.html")
	if err != nil {
		http.Error(w, "ui unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page names its scripts/styles by fixed paths: always revalidate so
	// an upgraded binary's UI shows up on the next load.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// staticTypes pins the Content-Type of what static/ holds, so it never
// depends on the host's mime tables (a bare Pi has none for .woff2).
var staticTypes = map[string]string{
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".woff2": "font/woff2",
	".svg":   "image/svg+xml",
	".md":    "text/markdown; charset=utf-8",
}

// staticFiles serves web.Static; staticETags holds a content hash per file
// (computed once) so a no-cache revalidation is a cheap 304.
var (
	staticFiles     = http.StripPrefix("/static/", http.FileServerFS(web.Static))
	staticETagsOnce sync.Once
	staticETags     map[string]string
)

func staticETag(name string) string {
	staticETagsOnce.Do(func() {
		staticETags = map[string]string{}
		_ = fs.WalkDir(web.Static, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if b, err := fs.ReadFile(web.Static, p); err == nil {
				sum := sha256.Sum256(b)
				staticETags[p] = `"` + hex.EncodeToString(sum[:8]) + `"`
			}
			return nil
		})
	})
	return staticETags[name]
}

// handleStatic serves the UI's embedded assets under /static/. Only files: a
// missing path or a directory (no listings) is a plain 404 — never the SPA
// page, so a broken asset reference fails loudly. Scripts and styles are
// revalidated on every load (they change with the binary); fonts never change
// under a name, so they're cached for a year.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" || !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	info, err := fs.Stat(web.Static, name)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := staticTypes[ext]; ok {
		w.Header().Set("Content-Type", ct)
	}
	if ext == ".woff2" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if tag := staticETag(name); tag != "" {
		w.Header().Set("ETag", tag)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	staticFiles.ServeHTTP(w, r)
}

// --- auth / provisioning --------------------------------------------------
//
// The agent web UI is a small state machine gated by GET /api/session:
//
//   unconfigured   → no store key             → "paste a token"     (POST /api/setup/token)
//   needs_password → token linked, no password→ "create a password" (POST /api/setup/password)
//   locked         → token+password, no session→ "enter password"   (POST /api/login)
//   ready          → token+password+valid session → the app
//
// The setup + login endpoints are LAN-open — there's no session to gate them
// with before one exists. Each enforces its own state precondition so they
// can't be driven out of order.

// bcryptCost is the work factor for the local UI password. DefaultCost (10) is
// plenty for a LAN-gated, single-password device.
const bcryptCost = bcrypt.DefaultCost

type setupTokenDTO struct {
	Token string `json:"token"`
}

// handleSetupToken provisions a blank agent from an admin-minted token. It
// verifies the token against main synchronously (a device-authed GET — no SSE
// race), and only on success persists it + points the SSE controller at it.
// Valid only when unconfigured; a rejected token is reported inline and nothing
// is stored.
func (s *Server) handleSetupToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	existing, err := s.settings.GetStoreAPIKey(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if existing != "" {
		s.writeJSON(w, http.StatusConflict, errCodeMap("This device is already linked.", "already_provisioned"))
		return
	}
	var body setupTokenDTO
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("Paste the token from the manager dashboard.", "token_required"))
		return
	}
	// Verify against main before storing anything: a device-authed GET returns
	// 200 for a live key, 401 for an unknown/revoked one.
	status, _, err := s.mainClient.Get(ctx, token, "/store/v1/store-config", "")
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("Can't reach the server — check the connection and try again.", "server_unreachable"))
		return
	}
	switch {
	case status == http.StatusUnauthorized:
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("That token was rejected by the server — check it and try again.", "invalid_token"))
		return
	case status < 200 || status >= 300:
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("The server refused the token — try again.", "verify_failed"))
		return
	}
	// Valid — persist + connect. Next state is needs_password.
	if err := s.settings.SetStoreAPIKey(ctx, token); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if s.sseController != nil {
		s.sseController.SetKey(token)
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type setupPasswordDTO struct {
	Password string `json:"password"`
	Confirm  string `json:"confirm"`
	Hint     string `json:"hint"`
}

// handleSetupPassword sets the initial local UI password (+ optional hint) right
// after token provisioning, then issues a session. Valid only in the
// needs_password state (token linked, no password yet).
func (s *Server) handleSetupPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key, err := s.settings.GetStoreAPIKey(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if key == "" {
		s.writeJSON(w, http.StatusConflict, errCodeMap("Link a token first.", "not_provisioned"))
		return
	}
	hash, _, err := s.settings.GetUIPassword(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if hash != "" {
		s.writeJSON(w, http.StatusConflict, errCodeMap("A password is already set.", "password_already_set"))
		return
	}
	var body setupPasswordDTO
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if body.Password == "" {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("Choose a password.", "password_required"))
		return
	}
	if body.Password != body.Confirm {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("The passwords don't match.", "password_mismatch"))
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcryptCost)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := s.settings.SetUIPassword(ctx, string(newHash), strings.TrimSpace(body.Hint)); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.session.issue(w)
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type passwordLoginDTO struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin unlocks the web UI with the local password. Valid only when the
// agent is fully provisioned (token + password); otherwise the UI belongs on the
// setup / create-password screen, so we send it back with not_provisioned.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key, err := s.settings.GetStoreAPIKey(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	hash, _, err := s.settings.GetUIPassword(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if key == "" || hash == "" {
		s.writeJSON(w, http.StatusConflict, errCodeMap("This device isn't set up yet.", "not_provisioned"))
		return
	}
	var body passwordLoginDTO
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	ip := clientIP(r)
	release, wait := s.logins.begin(ip, time.Now())
	if wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		s.writeJSON(w, http.StatusTooManyRequests, errCodeMap("Too many wrong passwords — wait a minute and try again.", "too_many_attempts"))
		return
	}
	defer release()
	ok := s.loginUsernameOK(r, body.Username) && bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) == nil
	if !ok && s.recovery != nil {
		// The manager may have changed the password in main since the last
		// sync: refresh the hash once (online only) and try again.
		if changed, _ := s.recovery.RefreshManagerHash(ctx); changed {
			if h, _, err := s.settings.GetUIPassword(ctx); err == nil {
				ok = s.loginUsernameOK(r, body.Username) && bcrypt.CompareHashAndPassword([]byte(h), []byte(body.Password)) == nil
			}
		}
	}
	if !ok {
		s.logins.fail(ip, time.Now())
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("Incorrect password.", "invalid_password"))
		return
	}
	s.logins.ok(ip)
	s.session.issue(w)
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request) {
	s.session.clear(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleHint returns the (optional) password hint for the login screen's
// "Hint?" link. LAN-open: it's a reminder the operator chose, not a secret.
func (s *Server) handleHint(w http.ResponseWriter, r *http.Request) {
	_, hint, err := s.settings.GetUIPassword(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"hint": hint})
}

// handleChangePassword (Settings → Change password) rotates the local UI
// password after verifying the current one. The optional hint is replaced too.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if li, err := s.settings.GetLinkInfo(ctx); err == nil && li.ManagerUsername != "" {
		s.writeJSON(w, http.StatusConflict, errCodeMap("This Pi unlocks with the store manager login — change that password in the manager dashboard (Store → Login).", "manager_login"))
		return
	}
	hash, _, err := s.settings.GetUIPassword(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
		Confirm string `json:"confirm"`
		Hint    string `json:"hint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Current)) != nil {
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("Current password is incorrect.", "invalid_password"))
		return
	}
	if body.New == "" {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("Choose a new password.", "password_required"))
		return
	}
	if body.New != body.Confirm {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("The passwords don't match.", "password_mismatch"))
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(body.New), bcryptCost)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if err := s.settings.SetUIPassword(ctx, string(newHash), strings.TrimSpace(body.Hint)); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	// Sign out every other browser that knew the old password; keep this one.
	if err := s.session.rotate(); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("session rotation after password change failed")
	}
	s.session.issue(w)
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUnlink (Settings → Unlink device) revokes THIS device's key on main
// (self-revoke), wipes the local token + password, drops the SSE connection, and
// ends the session — resetting the agent to the "paste a token" setup screen.
// An already-dead key (main 401) still clears locally.
func (s *Server) handleUnlink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key, err := s.settings.GetStoreAPIKey(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if key != "" {
		status, rerr := s.mainClient.RevokeSelf(ctx, key)
		if rerr != nil {
			s.writeError(w, r, http.StatusBadGateway, rerr)
			return
		}
		if status != http.StatusNoContent && status != http.StatusUnauthorized {
			s.writeJSON(w, http.StatusBadGateway, errCodeMap("The server refused the unlink — try again.", "revoke_failed"))
			return
		}
	}
	if err := s.settings.ClearProvisioning(ctx); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if s.sseController != nil {
		s.sseController.SetKey("")
	}
	s.ResetAccess(ctx)
	s.session.clear(w)
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSession drives the whole UI state machine + the header connection light.
// `state` ∈ unconfigured | needs_password | locked | ready. `sse_state` (the
// green/amber/red dot) is derived separately from the live connection.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key, _ := s.settings.GetStoreAPIKey(ctx)
	hash, hint, _ := s.settings.GetUIPassword(ctx)
	configured := key != ""
	authed := s.session != nil && s.session.valid(r)
	st := s.sseController.Status()

	state := "unconfigured"
	switch {
	case key == "":
		state = "unconfigured"
	case hash == "":
		state = "needs_password"
	case authed:
		state = "ready"
	default:
		state = "locked"
	}

	sseState := s.sseState(configured)

	li, _ := s.settings.GetLinkInfo(ctx)
	host, _ := os.Hostname()
	sc := s.storeContext()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"store_name":          sc.StoreName(),
		"version":             s.version,
		"hostname":            host,
		"state":               state,
		"has_hint":            hint != "",
		"manager_login":       li.ManagerUsername != "",
		"link_available":      s.recovery != nil,
		"sse_state":           sseState,
		"sse_connected":       st.Connected,
		"sse_reconnects":      st.Reconnects,
		"sse_connected_since": st.ConnectedSince,
		"sse_last_event_at":   st.LastEventAt,
		"sse_last_error":      st.LastError,
	})
}

// --- historical orders (proxied to main/) ---------------------------------

// handleHistory relays GET /api/v1/orders on main/ using the stored store key,
// forwarding the query string (offset/num/q/status/from/to/sort) verbatim.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	key, err := s.settings.GetStoreAPIKey(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if key == "" {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "agent not configured — log in first"})
		return
	}
	status, respBody, err := s.mainClient.Get(r.Context(), key, "/api/v1/orders", r.URL.RawQuery)
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, err)
		return
	}
	s.relay(w, status, respBody)
}

// --- reprint / resend from a payload --------------------------------------

// handlePrint prints a snapshot the history page already fetched from main/,
// with no persistence or fan-out. Backs "Reprint" for orders older than the
// local window.
func (s *Server) handlePrint(w http.ResponseWriter, r *http.Request) {
	var snap order.Snapshot
	if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := s.service.PrintSnapshot(r.Context(), snap); err != nil {
		s.writeError(w, r, http.StatusBadGateway, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleResend runs the full arrival pipeline for a fetched snapshot ("Resend").
func (s *Server) handleResend(w http.ResponseWriter, r *http.Request) {
	var snap order.Snapshot
	if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := s.service.Resend(r.Context(), snap); err != nil {
		s.writeError(w, r, http.StatusBadGateway, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleResendByID re-runs the full pipeline for a locally-stored order.
func (s *Server) handleResendByID(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := s.service.ResendByID(r.Context(), id); err != nil {
		if errors.Is(err, order.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, err)
			return
		}
		s.writeError(w, r, http.StatusBadGateway, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- store-config view (read-only, proxied from main/) ---------------------

func (s *Server) handleGetStoreConfig(w http.ResponseWriter, r *http.Request) {
	key, err := s.settings.GetStoreAPIKey(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if key == "" {
		s.writeJSON(w, http.StatusConflict, map[string]string{"error": "agent not configured — log in first"})
		return
	}
	status, respBody, err := s.mainClient.Get(r.Context(), key, "/store/v1/store-config", "")
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, err)
		return
	}
	s.relay(w, status, respBody)
}

// relay writes a proxied upstream response (status + JSON body) verbatim.
func (s *Server) relay(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ResetAccess ends every web session and revokes every paired tablet — on
// unlink and on a revoke-driven wipe, when the agent stops belonging to its
// location (it may be re-linked somewhere else next).
func (s *Server) ResetAccess(ctx context.Context) {
	if err := s.session.rotate(); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("session rotation failed")
	}
	if s.devices != nil {
		if err := s.devices.RevokeAll(ctx, time.Now()); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("revoking paired tablets failed")
		}
	}
}

// loginThrottle slows password guessing on the LAN: after 5 wrong passwords
// from one address, that address waits 30 s, doubling per further miss (max
// 15 min). A correct password clears it.
type loginThrottle struct {
	mu     sync.Mutex
	fails  map[string]*loginFails
	active map[string]bool // an attempt from this IP is being checked
}

type loginFails struct {
	n     int
	until time.Time
}

func (t *loginThrottle) blockedFor(ip string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if f := t.fails[ip]; f != nil && now.Before(f.until) {
		return f.until.Sub(now)
	}
	return 0
}

// begin reserves ip's single in-flight attempt; the caller must call the
// returned release when the attempt is decided. A failure is only counted
// after bcrypt (and maybe a hash refresh from main), so without this N
// parallel requests got N guesses past the block check. wait > 0 → refuse.
func (t *loginThrottle) begin(ip string, now time.Time) (release func(), wait time.Duration) {
	if wait := t.blockedFor(ip, now); wait > 0 {
		return func() {}, wait
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active[ip] {
		return func() {}, time.Second
	}
	if t.active == nil {
		t.active = map[string]bool{}
	}
	t.active[ip] = true
	return func() {
		t.mu.Lock()
		delete(t.active, ip)
		t.mu.Unlock()
	}, 0
}

func (t *loginThrottle) fail(ip string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fails == nil {
		t.fails = map[string]*loginFails{}
	}
	f := t.fails[ip]
	if f == nil {
		f = &loginFails{}
		t.fails[ip] = f
	}
	f.n++
	if f.n >= 5 {
		d := 30 * time.Second << min(f.n-5, 5)
		if d > 15*time.Minute {
			d = 15 * time.Minute
		}
		f.until = now.Add(d)
	}
}

func (t *loginThrottle) ok(ip string) {
	t.mu.Lock()
	delete(t.fails, ip)
	t.mu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
