package terminal

import (
	"encoding/json"
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/terminal"
)

// TestGeneralRefund (contract 2.10x): cash back with no order - a manager
// every time (the owner, 2026-11-12), cash only, a reason, idempotent by client_key,
// the drawer's expected cash, refund.general to main, the journal.
func TestGeneralRefund(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234") // Cara: cashier
	if _, err := r.svc.GeneralRefund(r.ctx, till, GeneralRefundIn{ClientKey: tk(), AmountCents: 500}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("no reason: %v", err)
	}
	if _, err := r.svc.GeneralRefund(r.ctx, till, GeneralRefundIn{ClientKey: tk(), Tender: "card", AmountCents: 500, Reason: "x"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("card: %v", err)
	}
	if _, err := r.svc.GeneralRefund(r.ctx, till, GeneralRefundIn{AmountCents: 500, Reason: "x"}); !errors.Is(err, terminal.ErrClientKeyRequired) {
		t.Fatalf("no key: %v", err)
	}
	var mr *terminal.ManagerRequired
	// A manager every time: even a cent, even under the cashier refund limit.
	if _, err := r.svc.GeneralRefund(r.ctx, till, GeneralRefundIn{ClientKey: tk(), AmountCents: 1, Reason: "spilled"}); !errors.As(err, &mr) ||
		mr.Action != terminal.ActionRefund || mr.Limit != 0 || mr.Requested != 1 {
		t.Fatalf("a cashier alone: %v", err)
	}
	if _, err := r.svc.GeneralRefund(r.ctx, till, GeneralRefundIn{ClientKey: tk(), AmountCents: 1001, Reason: "spilled", ManagerPIN: "1234"}); !errors.Is(err, terminal.ErrManagerPINWrong) {
		t.Fatalf("a cashier's PIN: %v", err)
	}

	key := tk()
	in := GeneralRefundIn{ClientKey: key, AmountCents: 1000, Reason: "Spilled drink", ManagerPIN: "9876"}
	res, err := r.svc.GeneralRefund(r.ctx, till, in)
	if err != nil {
		t.Fatal(err)
	}
	p := res.Payment
	if res.Duplicate || !res.KickDrawer || p.OrderID != "" || p.RefundKind != terminal.RefundGeneral || !p.Refund || p.Tender != terminal.TenderCash ||
		p.AmountCents != 1000 || p.ApprovedBy != "mgr-1" || p.DrawerID == "" || p.BatchID == 0 || res.StaffName != "Cara S." {
		t.Fatalf("refund %+v", res)
	}
	// The same tap again: the same refund, the drawer stays shut.
	again, err := r.svc.GeneralRefund(r.ctx, till, in)
	if err != nil || !again.Duplicate || again.KickDrawer || again.Payment.ID != p.ID {
		t.Fatalf("again %+v %v", again, err)
	}
	var kr *terminal.KeyReused
	if _, err := r.svc.GeneralRefund(r.ctx, till, GeneralRefundIn{ClientKey: key, AmountCents: 900, Reason: "Spilled drink", ManagerPIN: "9876"}); !errors.As(err, &kr) {
		t.Fatalf("key reused: %v", err)
	}
	// A logged-in manager types nothing (on the second till).
	till2 := Actor{DeviceID: "dev-2", DeviceName: "Till 2"}
	r.login(t, till2, "9876")
	big, err := r.svc.GeneralRefund(r.ctx, till2, GeneralRefundIn{ClientKey: tk(), AmountCents: 2500, Reason: "Wrong change"})
	if err != nil || big.Payment.ApprovedBy != "" || big.Payment.DrawerID == p.DrawerID {
		t.Fatalf("manager %+v %v", big, err)
	}

	// Till 1's drawer: the start less its refund.
	d, err := r.repo.DrawerByDevice(r.ctx, till.DeviceID)
	if err != nil || d == nil {
		t.Fatalf("drawer %v %v", d, err)
	}
	if d.CashRefundsCents != 1000 || d.ExpectedCents != d.StartCents-1000 {
		t.Fatalf("drawer %+v", d)
	}
	// main: refund.general, the payment negative with its batch.
	evs, err := r.repo.ListUnsyncedEvents(r.ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, e := range evs {
		if e.Type != terminal.EventRefundGeneral {
			continue
		}
		n++
		var body struct {
			Payment map[string]any `json:"payment"`
			Reason  string         `json:"reason"`
			BatchID int64          `json:"batch_id"`
		}
		if err := json.Unmarshal(e.Payload, &body); err != nil {
			t.Fatal(err)
		}
		if e.OrderID != "" || e.Subject == "" || body.BatchID == 0 || body.Payment["amount_cents"].(float64) >= 0 || body.Payment["method"] != "cash" ||
			body.Reason == "" || body.Payment["client_key"] == nil {
			t.Fatalf("event %+v %+v", e, body)
		}
	}
	if n != 2 {
		t.Fatalf("%d refund.general events", n)
	}
	// The journal: refund {general}, the manager's override, the kicks.
	js, err := r.repo.Journal(r.ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var refunds, kicks, overrides int
	for _, e := range js {
		switch e.Action {
		case terminal.ActionRefund:
			refunds++
		case terminal.ActionDrawerKick:
			kicks++
		case terminal.ActionOverride:
			overrides++
		}
	}
	if refunds != 2 || kicks != 2 || overrides != 1 { // Cara's needed Khawar's PIN; Khawar's own none
		t.Fatalf("journal refunds %d kicks %d overrides %d", refunds, kicks, overrides)
	}
	if r.pub.count(EventGeneralRefund) != 2 {
		t.Fatalf("sse %v", r.pub.events)
	}
}
