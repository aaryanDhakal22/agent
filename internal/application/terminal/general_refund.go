package terminal

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// General refund (the owner, 2026-11-12; contract/agent.md §2.10x):
// POST /api/terminal/refunds. Cash given back with no order attached (the
// Management menu's "General refund" asks for no ticket number). Cash only;
// a cashier up to the cashier refund limit (cashier_refund_cents, default
// $20) per refund, above it a manager (manager_pin, an approval, or a
// logged-in manager). Idempotent by client_key (contract 2.10s).

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
	return terminal.Fingerprint(map[string]any{"tender": in.Tender, "amount_cents": in.AmountCents, "reason": strings.TrimSpace(in.Reason)})
}

// GeneralRefund gives cash back with no order (see the file comment).
func (s *Service) GeneralRefund(ctx context.Context, a Actor, in GeneralRefundIn) (*GeneralRefundResult, error) {
	ctx = WithApprovals(ctx, in.Approvals)
	res, err := s.generalRefund(ctx, a, in)
	result := resultOf(err)
	if err == nil && res.Duplicate {
		result = "duplicate"
	}
	s.cm.Payment(ctx, terminal.TenderCash, "general_refund", result)
	if err == nil && !res.Duplicate {
		p := res.Payment
		s.logger.Warn().Ctx(ctx).Str("payment_id", p.ID).Str("staff_id", p.StaffID).Str("staff_name", res.StaffName).
			Str("device_id", a.DeviceID).Str("tender", p.Tender).Int64("amount_cents", p.AmountCents).Str("drawer_id", p.DrawerID).
			Str("approver_id", p.ApprovedBy).Str("reason", p.Reason).Msg("General refund given")
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
	if in.Tender == "" {
		in.Tender = terminal.TenderCash
	}
	hash := generalFingerprint(in)
	// The same key again: the stored answer (or 409 key_reused) before any
	// limit is checked - a retried refund is not a second refund.
	if prev, err := s.repo.PaymentByKey(ctx, "", in.ClientKey); err != nil {
		return nil, err
	} else if prev != nil {
		if !terminal.SameRequest(prev.RequestHash, hash) {
			return nil, &terminal.KeyReused{ClientKey: in.ClientKey}
		}
		return s.generalResult(cfg, *prev, true), nil
	}
	if in.Tender != terminal.TenderCash {
		return nil, badRequest(terminal.ErrGeneralRefundCashOnly.Error())
	}
	if in.AmountCents < 1 || in.AmountCents > 10_000_00 {
		return nil, badRequest("amount_cents is 1-1000000")
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > 140 {
		return nil, badRequest("a general refund needs a reason (1-140 characters)")
	}
	pay := terminal.Payment{ID: uuid.Must(uuid.NewV7()).String(), ClientKey: in.ClientKey, At: s.now().UTC().Truncate(time.Millisecond),
		Tender: terminal.TenderCash, AmountCents: in.AmountCents, Refund: true, Reason: reason, StaffID: st.ID,
		DeviceID: a.DeviceID, RefundKind: terminal.RefundGeneral, RequestHash: hash}
	var approver *terminal.Staff
	need := terminal.GeneralRefundNeed(cfg, st, pay.AmountCents)
	if need != nil {
		if approver, err = s.manager(ctx, a, cfg, in.ManagerPIN, need); err != nil {
			return nil, err
		}
		pay.ApprovedBy = approver.ID
	}
	pay.DrawerStart = cfg.DrawerStartCents()
	d := map[string]any{"payment_id": pay.ID, "tender": pay.Tender, "amount_cents": pay.AmountCents, "tip_cents": 0,
		"reason": reason, "general": true, "client_key": pay.ClientKey}
	if approver != nil {
		d["approved_by"] = approver.ID
	}
	entries := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionRefund, Detail: detail(d)}}
	if approver != nil {
		entries = append(entries, overrideEntry(a, st, approver, need, ""))
	}
	entries = append(entries, kickEntry(a, st, "", "general_refund", pay.AmountCents))
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
