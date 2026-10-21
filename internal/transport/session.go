package transport

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// sessionCookie is the name of the agent web-UI session cookie. The value is
// "<expiryUnix>.<hex hmac>" signed with a secret that is now PERSISTED in the
// agent DB — so sessions survive agent restarts (they no longer log the manager
// out on every reboot / air rebuild). It's a browser-session cookie (no MaxAge →
// cleared on browser close), with a 24h absolute cap in the signed payload.
const sessionCookie = "agent_session"

// sessionTTL is the absolute cap: a login is forced to refresh after this even if
// the browser stays open.
const sessionTTL = 24 * time.Hour

// sessionManager mints + verifies signed session tokens.
type sessionManager struct {
	mu     sync.RWMutex
	secret []byte
	// persist saves a rotated secret (settings.session_secret), so a rotation
	// also holds across restarts.
	persist func(hexSecret string) error
}

// newSessionManager takes the persisted signing secret (resolved at boot). The
// secret must be stable across restarts for sessions to survive them.
func newSessionManager(secret []byte) *sessionManager {
	return &sessionManager{secret: secret}
}

// rotate replaces the signing secret, invalidating EVERY existing session
// cookie — on a password change, unlink, or a revoke-driven wipe, so an old
// browser session can't outlive the credentials it was issued for.
func (m *sessionManager) rotate() error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	if m.persist != nil {
		if err := m.persist(hex.EncodeToString(b)); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.secret = b
	m.mu.Unlock()
	return nil
}

func (m *sessionManager) sign(expiry int64) string {
	payload := strconv.FormatInt(expiry, 10)
	m.mu.RLock()
	defer m.mu.RUnlock()
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// issue writes a fresh session cookie onto the response. No MaxAge/Expires → a
// browser-session cookie that's dropped when the browser closes; the 24h cap
// lives in the signed payload (verified in valid()).
func (m *sessionManager) issue(w http.ResponseWriter) {
	expiry := time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    m.sign(expiry),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// clear expires the session cookie (logout).
func (m *sessionManager) clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// valid reports whether the request carries a non-expired, correctly-signed
// session cookie.
func (m *sessionManager) valid(r *http.Request) bool {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	dot := -1
	for i := 0; i < len(ck.Value); i++ {
		if ck.Value[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		return false
	}
	payload := ck.Value[:dot]
	expiry, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}
	expected := m.sign(expiry)
	return subtle.ConstantTimeCompare([]byte(ck.Value), []byte(expected)) == 1
}
