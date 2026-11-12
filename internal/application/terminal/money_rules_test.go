package terminal

import (
	"testing"

	"quiccpos/agent/internal/domain/terminal"
	mr "quiccpos/agent/internal/domain/terminal/moneyrules"
)

// The shared rule table's discounts and price changes through the Pi's own
// checks (discountNeed, checkPrices); refunds, general refunds and comps run
// in domain/terminal's TestMoneyRules.
func TestMoneyRulesDiscountsAndPrices(t *testing.T) {
	tab := mr.LoadRules(t, "../../domain/terminal/testdata/money-rules.json")
	for _, c := range tab.Discounts {
		cfg := tab.ConfigFor(t, c.Settings)
		need := discountNeed(cfg, tab.Staff(cfg, c.By), &Discount{Pct: c.Pct, Kind: c.Kind, Reason: "x"}, priced{manualCents: 1, manualPct: c.Pct})
		if got, want := mr.Answer(need, nil), mr.Want(t, c.Pi); got != want {
			t.Errorf("%s: the Pi answers %s, the table says %s", c.ID, got, want)
		}
	}
	for _, c := range tab.PriceChanges {
		cfg := tab.ConfigFor(t, c.Settings)
		ch := []terminal.PriceChange{{FromCents: c.From, ToCents: c.To, DropPct: terminal.DropPctOf(c.From, c.To), Comp: c.To == 0 && c.From > 0}}
		price, comp := checkPrices(cfg, tab.Staff(cfg, c.By), ch, nil)
		need := price
		if comp != nil {
			need = comp
		}
		if got, want := mr.Answer(need, nil), mr.Want(t, c.Pi); got != want {
			t.Errorf("%s (%s): the Pi answers %s, the table says %s", c.ID, c.Note, got, want)
		}
	}
}
