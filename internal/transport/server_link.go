package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"quiccpos/agent/internal/application/recovery"

	"github.com/google/uuid"
)

// Manager-login link (headless SD-card recovery). A blank agent is linked by
// the store manager login typed on the Quibase tablet (or the web UI): main
// mints the key, the open batch is restored, the calling tablet is paired, and
// the web UI unlocks with that same login from then on.

// SetRecovery wires the link service; onLinked runs after a successful link
// (cmd/main.go may restart the agent to pick up newly handed telemetry).
func (s *Server) SetRecovery(svc *recovery.Service, onLinked func(*recovery.Outcome)) {
	s.recovery = svc
	s.onLinked = onLinked
}

type setupLinkDTO struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	TabletName string `json:"tablet_name"`
}

// POST /api/setup/link {username, password, tablet_name?} — only while
// unconfigured. 200 {tenant, location, restored, device_key?}; 401
// invalid_credentials; 429 too_many_attempts; 502 needs_internet.
func (s *Server) handleSetupLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.recovery == nil {
		s.writeJSON(w, http.StatusNotImplemented, errCodeMap("Linking isn't available on this agent.", "not_available"))
		return
	}
	var body setupLinkDTO
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	if strings.TrimSpace(body.Username) == "" || body.Password == "" {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("Type the store manager username and password.", "credentials_required"))
		return
	}
	tablet := strings.TrimSpace(body.TabletName)
	if len(tablet) > 40 {
		tablet = tablet[:40]
	}
	ip := clientIP(r)
	if wait := s.logins.blockedFor(ip, time.Now()); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		s.writeJSON(w, http.StatusTooManyRequests, errCodeMap("Too many wrong passwords — wait a minute and try again.", "too_many_attempts"))
		return
	}
	// One link at a time: a double tap must not mint two agents.
	if !s.linkMu.TryLock() {
		s.writeJSON(w, http.StatusConflict, errCodeMap("Already linking — wait a moment.", "link_in_progress"))
		return
	}
	defer s.linkMu.Unlock()

	host, _ := os.Hostname()
	out, err := s.recovery.Link(ctx, body.Username, body.Password, host)
	switch {
	case errors.Is(err, recovery.ErrAlreadyLinked):
		s.writeJSON(w, http.StatusConflict, errCodeMap("This Pi is already linked.", "already_provisioned"))
		return
	case errors.Is(err, recovery.ErrBadLogin):
		s.logins.fail(ip, time.Now())
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("Wrong username or password.", "invalid_credentials"))
		return
	case errors.Is(err, recovery.ErrThrottled):
		s.writeJSON(w, http.StatusTooManyRequests, errCodeMap(err.Error(), "too_many_attempts"))
		return
	case errors.Is(err, recovery.ErrNoInternet):
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("This Pi can't reach the internet yet — check the cable/router. Trying again is safe.", "needs_internet"))
		return
	case err != nil:
		s.logger.Error().Ctx(ctx).Err(err).Msg("link failed")
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("Linking failed: "+err.Error(), "link_failed"))
		return
	}
	s.logins.ok(ip)
	if s.sseController != nil {
		s.sseController.SetKey(out.APIKey)
	}

	resp := map[string]any{"ok": true, "tenant": out.Tenant, "location": out.Location,
		"username": out.Username, "restored": out.Restored, "revoked_agents": out.Revoked}
	if tablet != "" && s.devices != nil {
		key, err := newDeviceKey()
		if err == nil {
			id := uuid.NewString()
			if err = s.devices.Create(ctx, id, tablet, hashDeviceKey(key), key[:len(deviceKeyPrefix)+6]); err == nil {
				resp["device_id"], resp["device_key"], resp["device_name"] = id, key, tablet
				s.logger.Info().Ctx(ctx).Str("device_id", id).Str("name", tablet).Msg("Tablet paired by the manager-login link")
			}
		}
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("link: pairing the tablet failed (pair it from the web UI)")
		}
	} else {
		s.session.issue(w) // linked from the web UI: that browser is signed in
	}
	if s.onLinked != nil {
		go s.onLinked(out)
	}
	s.writeJSON(w, http.StatusOK, resp)
}

type managerLoginDTO struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginUsernameOK: an agent linked with the manager login expects that
// username too (case-insensitive); one provisioned the old way has none.
func (s *Server) loginUsernameOK(r *http.Request, given string) bool {
	li, err := s.settings.GetLinkInfo(r.Context())
	if err != nil || li.ManagerUsername == "" || strings.TrimSpace(given) == "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(given), li.ManagerUsername)
}
