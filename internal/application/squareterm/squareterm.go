// Package squareterm is the Pi's side of Square on the counter terminal
// (contract/agent.md §2.10r): the Square Reader's sign-in, a typed card's
// payment and refunds of in-app card payments, all through main over the
// duplex channel (store-key auth). The store's Square OAuth lives in main;
// the Pi keeps the access token in memory only (never SQLite, never the log)
// and hands it to a terminal sealed under that terminal's device key.
package squareterm

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/safego"

	"github.com/rs/zerolog"
)

// The agent → main RPCs (main contract §2.5e).
const (
	MsgSession = "square.session"
	MsgKeyed   = "square.keyed_payment"
	MsgRefund  = "square.refund"
)

// SessionInfo is the HKDF info of the terminal's session key.
const SessionInfo = "quicc square session"

const (
	// sessionCacheFor: the Pi asks main again after this (the token may have
	// been refreshed or revoked meanwhile).
	sessionCacheFor = 10 * time.Minute
	// statusStaleAfter: GET /api/terminal/status refreshes the Square line in
	// the background when it is older than this.
	statusStaleAfter = 10 * time.Minute
	rpcTimeout       = 30 * time.Second
)

// Sender is the duplex channel (rpc.Registry.Send).
type Sender interface {
	Send(ctx context.Context, msgType string, payload any) (int, json.RawMessage, error)
}

var (
	// ErrUnsupported: main has no handler for the RPC (an older main: 202).
	ErrUnsupported = errors.New("the server can't do this yet")
	// ErrUnreachable: no answer from main, or main / Square failed: the
	// outcome is unknown (the same idempotency key is safe to send again).
	ErrUnreachable = errors.New("no answer from the server: the outcome is unknown")
	// ErrNotLinked: this Pi isn't linked to a store.
	ErrNotLinked = errors.New("this Pi isn't linked to a store")
)

// Refused is main's answer {ok: false, code}: Square isn't linked, the card
// was declined, the refund failed...
type Refused struct {
	Code       string
	Message    string
	SquareCode string
}

func (r *Refused) Error() string { return r.Message }

// HTTPStatus is the Pi's answer for a refusal (contract §2.10r).
func (r *Refused) HTTPStatus() int {
	switch r.Code {
	case "card_declined", "refund_failed":
		return http.StatusPaymentRequired
	case "square_not_linked", "square_reconnect":
		return http.StatusConflict
	case "bad_request":
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

// Session is the store's Square sign-in as main handed it out.
type Session struct {
	AppID       string
	LocationID  string
	Environment string
	AccessToken string
	ExpiresAt   *time.Time
	InPerson    *bool
}

// StatusView is the "square" line of GET /api/terminal/status (never the
// token).
type StatusView struct {
	Linked      bool       `json:"linked"`
	Environment string     `json:"environment,omitempty"`
	LocationID  string     `json:"location_id,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	InPerson    *bool      `json:"in_person,omitempty"`
	Code        string     `json:"code,omitempty"`
	CheckedAt   time.Time  `json:"checked_at"`
}

// Service talks to main for the terminals.
type Service struct {
	rpc    func() Sender
	logger zerolog.Logger
	now    func() time.Time

	mu       sync.Mutex
	session  *Session
	gotAt    time.Time
	status   *StatusView
	checking bool
}

// New builds the service; rpc returns the duplex channel (nil = not wired).
func New(rpc func() Sender, lg zerolog.Logger) *Service {
	return &Service{rpc: rpc, logger: lg.With().Str("module", "square").Logger(), now: time.Now}
}

// reply is main's answer to every Square RPC.
type reply struct {
	OK          bool            `json:"ok"`
	Code        string          `json:"code"`
	Error       string          `json:"error"`
	SquareCode  string          `json:"square_code"`
	AppID       string          `json:"app_id"`
	LocationID  string          `json:"location_id"`
	Environment string          `json:"environment"`
	AccessToken string          `json:"access_token"`
	ExpiresAt   *time.Time      `json:"expires_at"`
	InPerson    *bool           `json:"in_person"`
	Payment     json.RawMessage `json:"payment"`
	RefundID    string          `json:"refund_id"`
	Status      string          `json:"status"`
}

// send asks main and sorts the answer: ok, a refusal, or an error.
func (s *Service) send(ctx context.Context, typ string, payload any) (*reply, error) {
	var snd Sender
	if s.rpc != nil {
		snd = s.rpc()
	}
	if snd == nil {
		return nil, ErrNotLinked
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	status, raw, err := snd.Send(ctx, typ, payload)
	switch {
	case err != nil:
		s.logger.Warn().Ctx(ctx).Err(err).Str("rpc", typ).Msg("square rpc: no answer from main")
		return nil, ErrUnreachable
	case status == http.StatusAccepted:
		return nil, ErrUnsupported
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, ErrNotLinked
	case status == http.StatusBadRequest:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return nil, &Refused{Code: "bad_request", Message: strings.TrimSpace("The server refused the request. " + e.Error)}
	case status != http.StatusOK:
		s.logger.Warn().Ctx(ctx).Int("status", status).Str("rpc", typ).Msg("square rpc: main failed (outcome unknown)")
		return nil, ErrUnreachable
	}
	var r reply
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, ErrUnreachable
	}
	if !r.OK {
		msg := r.Error
		if msg == "" {
			msg = "Square refused it (" + r.Code + ")"
		}
		return &r, &Refused{Code: r.Code, Message: msg, SquareCode: r.SquareCode}
	}
	return &r, nil
}

// Session is the store's Square sign-in (cached in memory for a few minutes;
// never written anywhere).
func (s *Service) Session(ctx context.Context) (Session, error) {
	s.mu.Lock()
	if c := s.session; c != nil && s.now().Sub(s.gotAt) < sessionCacheFor && (c.ExpiresAt == nil || s.now().Before(c.ExpiresAt.Add(-time.Hour))) {
		out := *c
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()
	r, err := s.send(ctx, MsgSession, map[string]any{})
	now := s.now()
	var ref *Refused
	switch {
	case errors.As(err, &ref):
		s.mu.Lock()
		s.session = nil
		v := &StatusView{Linked: false, Code: ref.Code, CheckedAt: now.UTC()}
		if r != nil {
			v.InPerson = r.InPerson
		}
		s.status = v
		s.mu.Unlock()
		return Session{}, err
	case err != nil:
		return Session{}, err
	}
	if r.AccessToken == "" || r.LocationID == "" {
		return Session{}, &Refused{Code: "square_not_linked", Message: "The store's Square isn't connected in main."}
	}
	sess := Session{AppID: r.AppID, LocationID: r.LocationID, Environment: r.Environment, AccessToken: r.AccessToken,
		ExpiresAt: r.ExpiresAt, InPerson: r.InPerson}
	s.mu.Lock()
	s.session, s.gotAt = &sess, now
	s.status = &StatusView{Linked: true, Environment: sess.Environment, LocationID: sess.LocationID, ExpiresAt: sess.ExpiresAt,
		InPerson: sess.InPerson, CheckedAt: now.UTC()}
	s.mu.Unlock()
	return sess, nil
}

// Forget drops the cached session (a terminal said Square refused it).
func (s *Service) Forget() {
	s.mu.Lock()
	s.session = nil
	s.mu.Unlock()
}

// Status is the last known Square line for GET /api/terminal/status (nil =
// never asked); a stale one is refreshed in the background.
func (s *Service) Status() *StatusView {
	s.mu.Lock()
	defer s.mu.Unlock()
	stale := s.status == nil || s.now().Sub(s.status.CheckedAt) > statusStaleAfter
	if stale && !s.checking && s.rpc != nil && s.rpc() != nil {
		s.checking = true
		safego.Go("square.status", func() {
			defer func() {
				s.mu.Lock()
				s.checking = false
				s.mu.Unlock()
			}()
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			defer cancel()
			_, _ = s.Session(ctx)
		})
	}
	if s.status == nil {
		return nil
	}
	v := *s.status
	return &v
}

// --- the terminal's sealed session --------------------------------------------------

// SessionKey is HKDF-SHA256(ikm = the device key, no salt, info =
// SessionInfo), 32 bytes (RFC 5869: the empty salt is HashLen zeros).
func SessionKey(deviceKey string) []byte {
	prk := hmacSHA256(make([]byte, sha256.Size), []byte(deviceKey))
	return hmacSHA256(prk, append([]byte(SessionInfo), 1))[:32]
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// Seal is AES-256-GCM(token) under SessionKey(deviceKey) with nonce (12
// bytes): ciphertext ‖ 16-byte tag, as Java's AES/GCM/NoPadding writes it.
func Seal(deviceKey string, nonce []byte, token string) ([]byte, error) {
	block, err := aes.NewCipher(SessionKey(deviceKey))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, errors.New("square session: the nonce must be 12 bytes")
	}
	return gcm.Seal(nil, nonce, []byte(token), nil), nil
}

// Open reverses Seal (tests; the terminal opens it in Kotlin).
func Open(deviceKey string, nonce, sealed []byte) (string, error) {
	block, err := aes.NewCipher(SessionKey(deviceKey))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	return string(plain), err
}

// SealedSession is GET /api/terminal/square/session's answer.
type SealedSession struct {
	AppID          string     `json:"app_id"`
	LocationID     string     `json:"location_id"`
	Environment    string     `json:"environment"`
	AccessTokenEnc string     `json:"access_token_enc"`
	Nonce          string     `json:"nonce"`
	ExpiresAt      *time.Time `json:"expires_at"`
	InPerson       *bool      `json:"in_person,omitempty"`
}

// SealFor seals the session for the terminal holding deviceKey, with a fresh
// nonce.
func SealFor(sess Session, deviceKey string) (SealedSession, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return SealedSession{}, err
	}
	ct, err := Seal(deviceKey, nonce, sess.AccessToken)
	if err != nil {
		return SealedSession{}, err
	}
	return SealedSession{AppID: sess.AppID, LocationID: sess.LocationID, Environment: sess.Environment,
		AccessTokenEnc: base64.StdEncoding.EncodeToString(ct), Nonce: base64.StdEncoding.EncodeToString(nonce),
		ExpiresAt: sess.ExpiresAt, InPerson: sess.InPerson}, nil
}

// --- a typed card ---------------------------------------------------------------------

// KeyedIn is POST /api/terminal/square/keyed-payments.
type KeyedIn struct {
	Nonce          string `json:"nonce"`
	AmountCents    int64  `json:"amount_cents"`
	TipCents       int64  `json:"tip_cents"`
	IdempotencyKey string `json:"idempotency_key"`
	ReferenceID    string `json:"reference_id"`
	OrderID        string `json:"order_id"`
	// Fulfillment (2.10ao): the terminal's / call center's order type for a
	// cart not sent yet (delivery | pickup | counter). Only a hint: a sent
	// order's own fulfilment decides.
	Fulfillment string `json:"fulfillment,omitempty"`
	// Hold (2.10ao) is the Pi's decision, never the client's (the handler
	// overwrites it): a delivery's card is authorized only (main
	// square.keyed_payment hold: true).
	Hold bool `json:"hold"`
}

// KeyedPayment is the answer's payment.
type KeyedPayment struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	Brand         string `json:"brand"`
	Last4         string `json:"last4"`
	AuthCode      string `json:"auth_code"`
	ReceiptNumber string `json:"receipt_number"`
	Entry         string `json:"entry"`
	AmountCents   int64  `json:"amount_cents"`
	TipCents      int64  `json:"tip_cents"`
	// Held (2.10ao): Square only authorized it (status APPROVED, a delivery's
	// hold); the terminal records the payment with held: true.
	Held bool `json:"held,omitempty"`
}

// Check validates a keyed payment body (the Pi's own limits).
func (in KeyedIn) Check() error {
	switch {
	case strings.TrimSpace(in.Nonce) == "" || len(in.Nonce) > 512:
		return &Refused{Code: "bad_request", Message: "nonce (Square's card field token) is required"}
	case in.AmountCents < 1 || in.AmountCents > 1_000_000 || in.TipCents < 0 || in.TipCents > 100_000:
		return &Refused{Code: "bad_request", Message: "amount_cents is 1-1000000, tip_cents 0-100000"}
	case strings.TrimSpace(in.IdempotencyKey) == "" || len(in.IdempotencyKey) > 45:
		return &Refused{Code: "bad_request", Message: "idempotency_key is 1-45 characters"}
	case len(in.ReferenceID) > 64 || len(in.OrderID) > 64:
		return &Refused{Code: "bad_request", Message: "reference_id and order_id are at most 64 characters (Square keeps 40)"}
	}
	return nil
}

// Keyed has main create the payment for a typed card. Nothing about the card
// is kept: the answer goes back to the terminal, which records it.
func (s *Service) Keyed(ctx context.Context, in KeyedIn) (KeyedPayment, error) {
	if err := in.Check(); err != nil {
		return KeyedPayment{}, err
	}
	r, err := s.send(ctx, MsgKeyed, in)
	if err != nil {
		return KeyedPayment{}, err
	}
	var p KeyedPayment
	if json.Unmarshal(r.Payment, &p) != nil || p.ID == "" {
		return KeyedPayment{}, ErrUnreachable
	}
	p.Held = p.Status == "APPROVED"
	return p, nil
}

// --- refunds -------------------------------------------------------------------------

// RefundResult is Square's refund.
type RefundResult struct {
	RefundID string
	Status   string
}

// RefundKey is the Square idempotency key of a terminal refund: one per
// (order, client_key), ≤ 45 characters, so the manager's retry after no
// answer is the same refund.
func RefundKey(orderID, clientKey string) string {
	sum := sha256.Sum256([]byte("terminal-refund\x00" + orderID + "\x00" + clientKey))
	return "tr" + hex.EncodeToString(sum[:])[:40]
}

// Refund has main refund (part of) a Square payment. Recorded by the caller
// only on success.
func (s *Service) Refund(ctx context.Context, paymentID string, cents int64, key, reason string) (RefundResult, error) {
	r, err := s.send(ctx, MsgRefund, map[string]any{"payment_id": paymentID, "amount_cents": cents, "idempotency_key": key, "reason": reason})
	if err != nil {
		return RefundResult{}, err
	}
	if r.RefundID == "" {
		return RefundResult{}, ErrUnreachable
	}
	return RefundResult{RefundID: r.RefundID, Status: r.Status}, nil
}

// MsgTipAdjust (batch F, contract 2.10am / main 2.5e): put a driver's typed
// card tip on a counter card payment at the press of Close batch.
const MsgTipAdjust = "square.tip_adjust"

// TipAdjustKey is the Square idempotency key of one tip adjustment (one per
// payment and whole tip, ≤ 45 characters).
func TipAdjustKey(paymentID string, tipCents int64) string {
	sum := sha256.Sum256([]byte("driver-tip\x00" + paymentID + "\x00" + strconv.FormatInt(tipCents, 10)))
	return "dt" + hex.EncodeToString(sum[:])[:40]
}

// TipAdjust has main set a Square payment's whole tip. A *Refused with code
// not_adjustable = Square no longer takes a tip change on it (completed).
func (s *Service) TipAdjust(ctx context.Context, squarePaymentID string, tipCents int64, orderID string) (string, error) {
	r, err := s.send(ctx, MsgTipAdjust, map[string]any{"square_payment_id": squarePaymentID, "tip_cents": tipCents,
		"idempotency_key": TipAdjustKey(squarePaymentID, tipCents), "order_id": orderID})
	if err != nil {
		return "", err
	}
	return r.Status, nil
}
