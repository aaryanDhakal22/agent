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
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/device"

	"github.com/google/uuid"
)

// Tablet pairing + device-key auth.
//
// The web UI (session) asks for a 6-digit code (POST /api/pairing/code, 5-min
// TTL, one live code at a time, burned after 5 wrong guesses); the tablet
// sends it with a name to POST /api/pair and gets a device key back once. The
// key (qagt_…) goes in X-Agent-Device-Key on every floor request; only its
// sha256 is stored. Devices are listed + revoked in the web UI's Settings.

const (
	deviceKeyHeader  = "X-Agent-Device-Key"
	deviceKeyPrefix  = "qagt_"
	pairingTTL       = 5 * time.Minute
	pairingMaxMisses = 5
	touchEvery       = time.Minute
)

type pairingCodes struct {
	mu      sync.Mutex
	code    string
	expires time.Time
	misses  int
}

func (p *pairingCodes) issue(now time.Time) (string, time.Time, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", time.Time{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.code = fmt.Sprintf("%06d", n.Int64())
	p.expires = now.Add(pairingTTL)
	p.misses = 0
	return p.code, p.expires, nil
}

// consume reports whether code is the live one; success or too many misses
// burns it.
func (p *pairingCodes) consume(code string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code == "" || now.After(p.expires) {
		p.code = ""
		return false
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(p.code)) == 1 {
		p.code = ""
		return true
	}
	p.misses++
	if p.misses >= pairingMaxMisses {
		p.code = ""
	}
	return false
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
// web-UI session (the backup accept screen).
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
			h(w, r.WithContext(context.WithValue(r.Context(), deviceCtxKey{}, d)))
			return
		}
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("pair this device first (or log in)", "device_required"))
	})
}

// POST /api/pairing/code (session) — show a fresh 6-digit code.
func (s *Server) handlePairingCode(w http.ResponseWriter, _ *http.Request) {
	code, exp, err := s.pairing.issue(time.Now())
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, errCodeMap("could not generate a code", "internal"))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires_at": exp.UTC()})
}

// POST /api/pair {code, name} (open) — exchange a code for a device key.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 40 {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("name must be 1-40 characters", "name_required"))
		return
	}
	if !s.pairing.consume(strings.TrimSpace(req.Code), time.Now()) {
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("wrong or expired pairing code", "invalid_code"))
		return
	}
	key, err := newDeviceKey()
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	id := uuid.NewString()
	if err := s.devices.Create(r.Context(), id, name, hashDeviceKey(key), key[:len(deviceKeyPrefix)+6]); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.logger.Info().Ctx(r.Context()).Str("device_id", id).Str("name", name).Msg("Tablet paired")
	s.writeJSON(w, http.StatusCreated, map[string]any{"device_id": id, "name": name, "key": key})
}

// GET /api/devices (session).
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	ds, err := s.devices.List(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, ds)
}

// POST /api/devices/{id}/revoke (session).
func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	err := s.devices.Revoke(r.Context(), r.PathValue("id"), time.Now())
	if errors.Is(err, device.ErrNotFound) {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("no such active device", "not_found"))
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
