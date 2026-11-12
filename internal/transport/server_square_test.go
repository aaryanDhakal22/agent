package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"quiccpos/agent/internal/application/squareterm"

	"github.com/rs/zerolog"
)

// squareMain answers the Square RPCs like main.
type squareMain struct {
	session map[string]any
}

func (m squareMain) Send(_ context.Context, typ string, _ any) (int, json.RawMessage, error) {
	if typ != squareterm.MsgSession || m.session == nil {
		return 202, nil, nil
	}
	raw, _ := json.Marshal(m.session)
	return 200, raw, nil
}

// GET /api/terminal/square/session (contract 2.10r §1): terminal keys only,
// the token sealed for the asking terminal, never in clear.
func TestTerminalSquareSession(t *testing.T) {
	s, h, tabletKey, termKey, logs := setupServer(t)
	if rec := call(h, "GET", "/api/terminal/square/session", termKey, ""); rec.Code != http.StatusServiceUnavailable || code(rec) != "not_linked" {
		t.Fatalf("no channel: %d %s", rec.Code, rec.Body)
	}
	m := squareMain{session: map[string]any{"ok": true, "access_token": "EAAA-store-token", "location_id": "L1", "environment": "sandbox",
		"app_id": "sandbox-sq0idb-x", "expires_at": "2026-12-12T00:00:00Z", "in_person": true}}
	s.square = squareterm.New(func() squareterm.Sender { return m }, zerolog.Nop())
	if rec := call(h, "GET", "/api/terminal/square/session", tabletKey, ""); rec.Code != http.StatusForbidden || code(rec) != "terminal_required" {
		t.Fatalf("tablet key: %d %s", rec.Code, rec.Body)
	}
	rec := call(h, "GET", "/api/terminal/square/session", termKey, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "EAAA-store-token") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("session: %d %s", rec.Code, rec.Body)
	}
	var got squareterm.SealedSession
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	nonce, _ := base64.StdEncoding.DecodeString(got.Nonce)
	ct, _ := base64.StdEncoding.DecodeString(got.AccessTokenEnc)
	if tok, err := squareterm.Open(termKey, nonce, ct); err != nil || tok != "EAAA-store-token" {
		t.Fatalf("the terminal opens %q, %v", tok, err)
	}
	if got.LocationID != "L1" || got.Environment != "sandbox" || got.AppID == "" || got.ExpiresAt == nil {
		t.Fatalf("session %+v", got)
	}
	if strings.Contains(logs.String(), "EAAA-store-token") {
		t.Fatal("the token is in the log")
	}
	// Square not connected in main: 409.
	s.square = squareterm.New(func() squareterm.Sender {
		return squareMain{session: map[string]any{"ok": false, "code": "square_not_linked", "error": "no Square"}}
	}, zerolog.Nop())
	if rec := call(h, "GET", "/api/terminal/square/session", termKey, ""); rec.Code != http.StatusConflict || code(rec) != "square_not_linked" {
		t.Fatalf("not linked: %d %s", rec.Code, rec.Body)
	}
	// An older main (202): 502 main_unsupported.
	s.square = squareterm.New(func() squareterm.Sender { return squareMain{} }, zerolog.Nop())
	if rec := call(h, "GET", "/api/terminal/square/session", termKey, ""); rec.Code != http.StatusBadGateway || code(rec) != "main_unsupported" {
		t.Fatalf("older main: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/terminal/square/keyed-payments", tabletKey, "{}"); rec.Code != http.StatusForbidden {
		t.Fatalf("keyed with a tablet key: %d %s", rec.Code, rec.Body)
	}
}
