package terminal

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/application/squareterm"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// General refund (the owner, 2026-11-12; contract/agent.md §2.10x):
// POST /api/terminal/refunds. Cash given back with no order attached (the
// Management menu's "General refund" asks for no ticket number). Cash, or
// (batch J, 2026-11-14) a card through Square; a manager every time (the owner, 2026-11-12: manager_pin, an approval or a
// logged-in manager; no cashier allowance). Idempotent by client_key
// (contract 2.10s).

// EventGeneralRefund is the terminals' SSE event {payment} after one.
const EventGeneralRefund = "terminal.refund.general"

// GeneralRefundIn is POST /api/terminal/refunds's body.
type GeneralRefundIn struct {
	ClientKey   string `json:"client_key"`
	Tender      string `json:"tender"` // cash (the only one; "" = cash)
	AmountCents int64  `json:"amount_cents"`
	Reason      string `json:"reason"`
	ManagerPIN  string `json:"manager_pin"`
	// Approvals (X-5): single-use manager approvals instead of manager_pin.
	Approvals []string `json:"approvals,omitempty"`
	// A card (batch J, 2026-11-14): exactly one of CardNonce (Square's card
	// field token: an unlinked refund; never stored or logged) and
	// SquarePaymentID (the fallback: a refund linked to that payment).
	CardNonce       string `json:"card_nonce,omitempty"`
	SquarePaymentID string `json:"square_payment_id,omitempty"`
	// Brand / Last4: what the card field / the listed payment showed
	// (Square's answer wins).
	Brand string `json:"brand,omitempty"`
	Last4 string `json:"last4,omitempty"`
}

// SquareGeneralRefunder gives money back to a card with no payment (Square's
// unlinked refund, squareterm.Service) and lists the store's recent card
// payments for the fallback.
type SquareGeneralRefunder interface {
	GeneralRefund(ctx context.Context, nonce string, cents int64, key, reason string) (squareterm.GeneralRefundResult, error)
	CardPayments(ctx context.Context, last4 string, cents int64) ([]squareterm.CardPayment, error)
}

// GeneralRefundResult is its answer.
type GeneralRefundResult struct {
	Payment   terminal.Payment `json:"payment"`
	Duplicate bool             `json:"duplicate"`
	// KickDrawer: cash goes back, open this terminal's drawer (never on a
	// duplicate: the first answer opened it).
	KickDrawer bool `json:"kick_drawer"`
	// StaffName / ApproverName: for the terminal's slip.
	StaffName    string `json:"staff_name"`
	ApproverName string `json:"approver_name,omitempty"`
}

// GeneralRefundRepo stores general refunds (repositories.OrderRepository).
type GeneralRefundRepo interface {
	AddGeneralRefund(ctx context.Context, p terminal.Payment, entries []terminal.Entry) (terminal.Payment, bool, error)
}

// ErrGeneralRefundsUnavailable: the storage keeps no general refunds.
var ErrGeneralRefundsUnavailable = errors.New("general refunds aren't available")

func generalFingerprint(in GeneralRefundIn) string {
	m := map[string]any{"tender": in.Tender, "amount_cents": in.AmountCents, "reason": strings.TrimSpace(in.Reason)}
	if in.Tender != terminal.TenderCash { // a card (never the token: a retry may hold a fresh one)
		m["square_payment_id"] = strings.TrimSpace(in.SquarePaymentID)
		m["unlinked"] = strings.TrimSpace(in.CardNonce) != ""
	}
	return terminal.Fingerprint(m)
}

// GeneralRefund gives cash back with no order (see the file comment).
func (s *Service) GeneralRefund(ctx context.Context, a Actor, in GeneralRefundIn) (*GeneralRefundResult, error) {
	ctx = WithApprovals(ctx, in.Approvals)
	res, err := s.generalRefund(ctx, a, in)
	result := resultOf(err)
	if err == nil && res.Duplicate {
		result = "duplicate"
	}
	tender := terminal.TenderCash
	if t, terr := terminal.GeneralRefundTender(in.Tender); terr == nil {
		tender = t
	}
	s.cm.Payment(ctx, tender, "general_refund", result)
	if err == nil && !res.Duplicate {
		p := res.Payment
		s.logger.Warn().Ctx(ctx).Str("payment_id", p.ID).Str("staff_id", p.StaffID).Str("staff_name", res.StaffName).
			Str("device_id", a.DeviceID).Str("tender", p.Tender).Int64("amount_cents", p.AmountCents).Str("drawer_id", p.DrawerID).
			Str("approver_id", p.ApprovedBy).Str("reason", p.Reason).Str("square_refund_id", p.SquareRefundID).Msg("General refund given")
		if res.KickDrawer {
			s.logger.Info().Ctx(ctx).Str("device_id", a.DeviceID).Str("reason", "general_refund").
				Int64("amount_cents", p.AmountCents).Msg("Drawer kicked")
		}
	}
	return res, err
}

func (s *Service) generalRefund(ctx context.Context, a Actor, in GeneralRefundIn) (*GeneralRefundResult, error) {
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	repo, ok := s.repo.(GeneralRefundRepo)
	if !ok {
		return nil, ErrGeneralRefundsUnavailable
	}
	if err := requireKey(in.ClientKey); err != nil {
		return nil, err
	}
	if !keyRe.MatchString(in.ClientKey) {
		return nil, badRequest("client_key is 8-64 letters, digits, _ . : -")
	}
	tender, err := terminal.GeneralRefundTender(in.Tender)
	if err != nil {
		return nil, badRequest(err.Error())
	}
	in.Tender = tender
	card := tender != terminal.TenderCash
	in.CardNonce, in.SquarePaymentID = strings.TrimSpace(in.CardNonce), strings.TrimSpace(in.SquarePaymentID)
	hash := generalFingerprint(in)
	// The same key again: the stored answer (or 409 key_reused) before any
	// limit is checked or Square is asked - a retried refund is not a second
	// refund.
	if prev, err := s.repo.PaymentByKey(ctx, "", in.ClientKey); err != nil {
		return nil, err
	} else if prev != nil {
		if !terminal.SameRequest(prev.RequestHash, hash) {
			return nil, &terminal.KeyReused{ClientKey: in.ClientKey}
		}
		return s.generalResult(cfg, *prev, true), nil
	}
	if in.AmountCents < 1 || in.AmountCents > 10_000_00 {
		return nil, badRequest("amount_cents is 1-1000000")
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > 140 {
		return nil, badRequest("a general refund needs a reason (1-140 characters)")
	}
	switch {
	case !card && (in.CardNonce != "" || in.SquarePaymentID != ""):
		return nil, badRequest("a cash general refund takes no card_nonce / square_payment_id")
	case card && (in.CardNonce == "") == (in.SquarePaymentID == ""):
		return nil, badRequest("a card general refund needs card_nonce (Square's card field) or square_payment_id (a listed payment), not both")
	case len(in.CardNonce) > 512 || len(in.SquarePaymentID) > 192:
		return nil, badRequest("card_nonce is at most 512 characters, square_payment_id 192")
	}
	last4 := strings.TrimSpace(in.Last4)
	if !card || !last4Re.MatchString(last4) {
		last4 = ""
	}
	pay := terminal.Payment{ID: uuid.Must(uuid.NewV7()).String(), ClientKey: in.ClientKey, At: s.now().UTC().Truncate(time.Millisecond),
		Tender: tender, AmountCents: in.AmountCents, Refund: true, Reason: reason, StaffID: st.ID,
		DeviceID: a.DeviceID, RefundKind: terminal.RefundGeneral, RequestHash: hash, Last4: last4}
	if card {
		pay.Brand = terminal.NormBrand(in.Brand)
	}
	var approver *terminal.Staff
	need := terminal.GeneralRefundNeed(cfg, st, pay.AmountCents)
	if need != nil {
		if approver, err = s.manager(ctx, a, cfg, in.ManagerPIN, need); err != nil {
			return nil, err
		}
		pay.ApprovedBy = approver.ID
	}
	if card {
		// Square first (batch J): recorded only once Square took it.
		if err := s.generalCardRefund(ctx, a, in, &pay); err != nil {
			return nil, err
		}
	}
	pay.DrawerStart = cfg.DrawerStartCents()
	d := map[string]any{"payment_id": pay.ID, "tender": pay.Tender, "amount_cents": pay.AmountCents, "tip_cents": 0,
		"reason": reason, "general": true, "client_key": pay.ClientKey}
	if card {
		d["square_refund_id"], d["unlinked"] = pay.SquareRefundID, in.CardNonce != ""
		if pay.SquarePaymentID != "" {
			d["square_payment_id"] = pay.SquarePaymentID
		}
	}
	if approver != nil {
		d["approved_by"] = approver.ID
	}
	entries := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionRefund, Detail: detail(d)}}
	if approver != nil {
		entries = append(entries, overrideEntry(a, st, approver, need, ""))
	}
	if !card { // a card never opens the drawer
		entries = append(entries, kickEntry(a, st, "", "general_refund", pay.AmountCents))
	}
	got, dup, err := repo.AddGeneralRefund(ctx, pay, entries)
	if err != nil {
		return nil, err
	}
	res := s.generalResult(cfg, got, dup)
	if !dup {
		s.publish(EventGeneralRefund, map[string]any{"payment": got})
	}
	return res, nil
}

func (s *Service) generalResult(cfg *terminal.Config, p terminal.Payment, dup bool) *GeneralRefundResult {
	res := &GeneralRefundResult{Payment: p, Duplicate: dup, KickDrawer: !dup && kicks([]terminal.Payment{p}) > 0}
	if st, ok := cfg.Find(p.StaffID); ok {
		res.StaffName = st.Name()
	}
	if p.ApprovedBy != "" {
		if st, ok := cfg.Find(p.ApprovedBy); ok {
			res.ApproverName = st.Name()
		}
	}
	return res
}

var last4Re = regexp.MustCompile(`^[0-9]{4}$`)

// generalCardRefund gives pay's money back on a card through Square (batch J,
// contract 2.10x "A general refund to a card"): the unlinked way
// (in.CardNonce: square.general_refund) or the fallback (in.SquarePaymentID:
// the ordinary linked square.refund). The Square idempotency key comes from
// the client_key, so a retry after no answer is the same Square refund. On
// success pay carries Square's refund id (ref + card block); on any error
// nothing is recorded. The call is journaled (never the token).
func (s *Service) generalCardRefund(ctx context.Context, a Actor, in GeneralRefundIn, pay *terminal.Payment) error {
	if s.square == nil {
		return ErrSquareUnavailable
	}
	key := squareterm.GeneralRefundKey(in.ClientKey)
	unlinked := in.CardNonce != ""
	var refundID, brand, last4 string
	var err error
	if unlinked {
		gs, ok := s.square.(SquareGeneralRefunder)
		if !ok {
			return ErrSquareUnavailable
		}
		var res squareterm.GeneralRefundResult
		if res, err = gs.GeneralRefund(ctx, in.CardNonce, pay.AmountCents, key, pay.Reason); err == nil {
			refundID, brand, last4 = res.RefundID, res.Brand, res.Last4
		}
	} else {
		var res squareterm.RefundResult
		if res, err = s.square.Refund(ctx, in.SquarePaymentID, pay.AmountCents, key, pay.Reason); err == nil {
			refundID = res.RefundID
		}
	}
	d := map[string]any{"amount_cents": pay.AmountCents, "idempotency_key": key, "unlinked": unlinked, "ok": err == nil,
		"client_key": in.ClientKey}
	if !unlinked {
		d["square_payment_id"] = in.SquarePaymentID
	}
	var ref *squareterm.Refused
	switch {
	case err == nil:
		d["square_refund_id"] = refundID
	case errors.As(err, &ref):
		d["code"], d["square_code"] = ref.Code, ref.SquareCode
	default:
		d["code"] = "unknown_outcome"
	}
	s.RecordPi(ctx, a.DeviceID, terminal.ActionSquareGeneralRefund, d)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Bool("unlinked", unlinked).Int64("amount_cents", pay.AmountCents).
			Msg("General card refund not given by Square (nothing recorded)")
		return err
	}
	pay.Ref = refundID
	pay.CardInfo = terminal.CardInfo{SquareRefundID: refundID, SquarePaymentID: in.SquarePaymentID}
	if unlinked {
		pay.Entry = "keyed"
	}
	if b := terminal.NormBrand(brand); b != "" {
		pay.Brand = b
	}
	if last4Re.MatchString(last4) {
		pay.Last4 = last4
	}
	return nil
}

// GeneralRefundCardPayments is the fallback's list (contract 2.10x, GET
// /api/terminal/refunds/card-payments): the store's Square card payments of
// the last 30 days with last4 that still hold cents. A staff login is
// enough to list (the manager's PIN comes with the refund).
func (s *Service) GeneralRefundCardPayments(ctx context.Context, a Actor, last4 string, cents int64) ([]squareterm.CardPayment, error) {
	if _, _, err := s.staffOf(a); err != nil {
		return nil, err
	}
	last4 = strings.TrimSpace(last4)
	if !last4Re.MatchString(last4) {
		return nil, badRequest("last4 is the card's last 4 digits")
	}
	if cents < 1 || cents > 10_000_00 {
		return nil, badRequest("amount_cents is 1-1000000")
	}
	if s.square == nil {
		return nil, ErrSquareUnavailable
	}
	gs, ok := s.square.(SquareGeneralRefunder)
	if !ok {
		return nil, ErrSquareUnavailable
	}
	return gs.CardPayments(ctx, last4, cents)
}
