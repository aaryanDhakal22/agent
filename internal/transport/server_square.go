package transport

import (
	"errors"
	"net/http"
	"strings"

	"quiccpos/agent/internal/application/squareterm"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/terminal"
)

// Square on the counter terminal, the Pi's routes (contract 2.10r): the
// reader's sign-in sealed for the asking terminal, a typed card's payment.
// Both go to main over the duplex channel; the token is kept in memory only.

// squareSvc is the Square service (built on the duplex channel by SetDiag).
func (s *Server) squareSvc() *squareterm.Service { return s.square }

// wireSquare builds the Square service on the RPC registry and hands the
// terminal flow its refunder. Called from SetDiag (the registry arrives
// there) and SetTerminal, whichever comes last finishes it.
func (s *Server) wireSquare() {
	if s.square == nil && s.diag.RPC != nil {
		reg := s.diag.RPC
		s.square = squareterm.New(func() squareterm.Sender { return reg }, s.logger)
	}
	if s.square != nil && s.terminalSvc != nil {
		s.terminalSvc.SetSquare(s.square)
	}
}

// squareTerminalKey is the asking terminal's device key (a terminal key
// only: the web session has none to seal for).
func squareTerminalKey(r *http.Request) (string, bool) {
	d := requestDevice(r)
	if d == nil || d.Kind != device.KindTerminal {
		return "", false
	}
	return r.Header.Get(deviceKeyHeader), true
}

// squareError answers a Square failure.
func (s *Server) squareError(w http.ResponseWriter, err error, unknownMsg string) {
	var ref *squareterm.Refused
	switch {
	case errors.As(err, &ref):
		body := map[string]any{"error": ref.Message, "code": ref.Code, "message": ref.Message}
		if ref.SquareCode != "" {
			body["square_code"] = ref.SquareCode
		}
		s.writeJSON(w, ref.HTTPStatus(), body)
	case errors.Is(err, squareterm.ErrUnsupported):
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("The server can't do Square for the terminal yet (update main).", "main_unsupported"))
	case errors.Is(err, squareterm.ErrNotLinked):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi isn't linked to a store.", "not_linked"))
	default:
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap(unknownMsg, "square_unavailable"))
	}
}

// GET /api/terminal/square/session → {app_id, location_id, environment,
// access_token_enc, nonce, expires_at, in_person?}: the store's Square
// sign-in for the reader, sealed for this terminal (AES-256-GCM under
// HKDF-SHA256(its device key, "quicc square session")). No staff login: the
// setup wizard signs the reader in before any PIN.
func (s *Server) handleTerminalSquareSession(w http.ResponseWriter, r *http.Request) {
	key, ok := squareTerminalKey(r)
	if !ok {
		s.writeJSON(w, http.StatusForbidden, errCodeMap("only a paired POS terminal can sign its reader in", "terminal_required"))
		return
	}
	sq := s.squareSvc()
	if sq == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi isn't linked to a store.", "not_linked"))
		return
	}
	sess, err := sq.Session(r.Context())
	if s.terminalSvc != nil {
		d := map[string]any{"ok": err == nil}
		var ref *squareterm.Refused
		if errors.As(err, &ref) {
			d["code"] = ref.Code
		} else if err != nil {
			d["code"] = "square_unavailable"
		}
		s.terminalSvc.RecordPi(r.Context(), actor(r).DeviceID, terminal.ActionSquareSession, d)
	}
	if err != nil {
		s.squareError(w, err, "Can't reach the server for Square right now: the reader stays signed in if it was.")
		return
	}
	out, err := squareterm.SealFor(sess, key)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, out)
}

// POST /api/terminal/square/keyed-payments {nonce, amount_cents, tip_cents?,
// idempotency_key, reference_id?, order_id?} → {payment: {id, status, brand,
// last4, auth_code, receipt_number, entry}}: main creates the payment for a
// card staff typed into Square's card field (MOTO). 402 card_declined
// {message, square_code}; 503 = the outcome is unknown (the same
// idempotency key is safe to send again). Needs a staff login.
func (s *Server) handleTerminalSquareKeyed(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	if _, ok := squareTerminalKey(r); !ok {
		s.writeJSON(w, http.StatusForbidden, errCodeMap("only a paired POS terminal can charge a card", "terminal_required"))
		return
	}
	sessView := s.terminalSvc.Session(actor(r))
	if sessView.Staff == nil {
		s.terminalError(w, r, terminal.ErrLoginRequired)
		return
	}
	var in squareterm.KeyedIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	in.Nonce, in.IdempotencyKey, in.ReferenceID = strings.TrimSpace(in.Nonce), strings.TrimSpace(in.IdempotencyKey), strings.TrimSpace(in.ReferenceID)
	sq := s.squareSvc()
	if sq == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi isn't linked to a store.", "not_linked"))
		return
	}
	p, err := sq.Keyed(r.Context(), in)
	d := map[string]any{"amount_cents": in.AmountCents, "tip_cents": in.TipCents, "reference_id": in.ReferenceID,
		"idempotency_key": in.IdempotencyKey, "staff_id": sessView.Staff.ID, "ok": err == nil}
	if in.OrderID != "" {
		d["order_id"] = in.OrderID
	}
	var ref *squareterm.Refused
	switch {
	case err == nil:
		d["square_payment_id"] = p.ID
	case errors.As(err, &ref):
		d["code"], d["square_code"] = ref.Code, ref.SquareCode
	default:
		d["code"] = "unknown_outcome"
	}
	s.terminalSvc.RecordPi(r.Context(), actor(r).DeviceID, terminal.ActionSquareKeyed, d)
	if err != nil {
		s.squareError(w, err, "No answer from Square: the card may have been charged. Check before charging again (the same card again is safe).")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"payment": p})
}

// squareStatus is the "square" line of GET /api/terminal/status (nil until
// the Pi has asked main once; refreshed in the background).
func (s *Server) squareStatus() *squareterm.StatusView {
	if sq := s.squareSvc(); sq != nil {
		return sq.Status()
	}
	return nil
}
