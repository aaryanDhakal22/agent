package terminal_test

import (
	"encoding/json"
	"testing"

	"quiccpos/agent/internal/domain/terminal"
	mr "quiccpos/agent/internal/domain/terminal/moneyrules"
)

// The shared rule table (quicc-ops contract/money-rules.json; this is its
// byte copy, scripts/sync-money-rules.sh): the Pi's answers as the table
// says. main and the terminal run the same file.

func TestMoneyRules(t *testing.T) {
	tab := mr.LoadRules(t, "testdata/money-rules.json")
	cfg := tab.ConfigFor(t, nil)
	pay := func(p mr.RulePay) terminal.Payment {
		x := terminal.Payment{Tender: p.Tender, AmountCents: p.Cents, TipCents: p.Tip, Refund: p.Refund, StaffID: p.By, RefundKind: p.Kind}
		if p.Approved {
			x.ApprovedBy = "mia"
		}
		return x
	}
	check := func(c mr.RuleCase, got string) {
		if want := mr.Want(t, c.Pi); got != want {
			t.Errorf("%s (%s): the Pi answers %s, the table says %s", c.ID, c.Note, got, want)
		}
	}

	// The effective cases are main's (it computes them); the Pi reads each
	// person's as main sends them:
	for name, p := range tab.People {
		if p.Absent {
			continue
		}
		l := cfg.LimitsOf(tab.Staff(cfg, name))
		var want struct {
			RefundCents int64 `json:"refund_cents"`
			DiscountPct int   `json:"discount_pct"`
		}
		unlimited := string(p.Effective) == "null" || len(p.Effective) == 0
		if !unlimited {
			_ = json.Unmarshal(p.Effective, &want)
		}
		if l.Unlimited != unlimited || (!unlimited && (l.RefundCents != want.RefundCents || l.DiscountPct != want.DiscountPct)) {
			t.Errorf("%s: the Pi reads limits %+v from main's effective %s", name, l, p.Effective)
		}
	}

	for _, c := range tab.Refunds {
		var paid []terminal.Payment
		for _, p := range c.Order.Payments {
			paid = append(paid, pay(p))
		}
		r := c.Refund
		r.Refund, r.By = true, c.By
		rp := pay(r)
		var got string
		earlier := !terminal.PayableBatch(c.Order.Batch == "" || c.Order.Batch == "open", c.Order.Batch == "carried")
		switch {
		case terminal.RefundRefused(paid, rp) != nil:
			got = mr.Answer(nil, terminal.RefundRefused(paid, rp))
		case earlier:
			got = mr.Answer(terminal.EarlierDayRefundNeed(cfg, tab.Staff(cfg, c.By), []terminal.Payment{rp}), nil)
		default:
			got = mr.Answer(terminal.RefundNeed(cfg, tab.Staff(cfg, c.By), paid, []terminal.Payment{rp}, terminal.ActionRefund), nil)
		}
		check(c, got)
	}
	for _, c := range tab.GeneralRefunds {
		check(c, mr.Answer(terminal.GeneralRefundNeed(cfg, tab.Staff(cfg, c.By), c.Cents), nil))
	}
	for _, c := range tab.Tips {
		p := pay(c.Payment)
		p.StaffID = c.By
		var err error
		switch {
		case c.TipAfter && !terminal.IsCard(p.Tender):
			err = terminal.ErrNotTippable
		case !c.TipAfter && !terminal.TipAllowed(p):
			err = terminal.ErrTipCardOnly
		}
		check(c, mr.Answer(nil, err))
	}
	for _, c := range tab.Comps {
		var need *terminal.ManagerRequired
		if !cfg.MayComp(tab.Staff(cfg, c.By)) { // the payment route's rule (application addPayment)
			need = &terminal.ManagerRequired{Action: terminal.ActionComp, Requested: c.Cents}
		}
		check(c, mr.Answer(need, nil))
	}
	// A void of a sent order (the whole order, a waste, a sent line taken
	// off): the service's rule for all three (application void / removalNeed).
	for _, c := range tab.Voids {
		check(c, mr.Answer(cfg.VoidNeed(tab.Staff(cfg, c.By), c.Cents), nil))
	}
}
