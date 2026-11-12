// Package moneyrules reads the shared money / permission rule table
// (quicc-ops contract/money-rules.json; the Pi's byte copy is
// internal/domain/terminal/testdata/money-rules.json) for the Pi's tests:
// domain/terminal's TestMoneyRules and application/terminal's
// TestMoneyRulesDiscountsAndPrices. Test helpers only.
package moneyrules

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"quiccpos/agent/internal/domain/terminal"
)

// RuleTable is the table as the Pi's tests read it (exported for the
// application package's test, which runs discounts and price changes).
type RuleTable struct {
	Settings map[string]any `json:"settings"`
	People   map[string]struct {
		Role      string          `json:"role"`
		Absent    bool            `json:"absent"`
		Effective json.RawMessage `json:"effective"`
	} `json:"people"`
	Effective []struct {
		ID       string         `json:"id"`
		Person   string         `json:"person"`
		Settings map[string]any `json:"settings"`
		Expect   *struct {
			RefundCents int64 `json:"refund_cents"`
			DiscountPct int   `json:"discount_pct"`
		} `json:"expect"`
	} `json:"effective"`
	Refunds        []RuleCase `json:"refunds"`
	GeneralRefunds []RuleCase `json:"general_refunds"`
	Comps          []RuleCase `json:"comps"`
	Tips           []RuleCase `json:"tips"`
	Discounts      []RuleCase `json:"discounts"`
	PriceChanges   []RuleCase `json:"price_changes"`
}

// RulePay is one payment of a case.
type RulePay struct {
	Tender   string `json:"tender"`
	Cents    int64  `json:"cents"`
	Tip      int64  `json:"tip"`
	Refund   bool   `json:"refund"`
	By       string `json:"by"`
	Approved bool   `json:"approved"`
	Kind     string `json:"kind"`
}

// RuleCase is one row of the table.
type RuleCase struct {
	ID       string         `json:"id"`
	Note     string         `json:"note"`
	By       string         `json:"by"`
	Cents    int64          `json:"cents"`
	Pct      int            `json:"pct"`
	Kind     string         `json:"kind"`
	From     int64          `json:"from"`
	To       int64          `json:"to"`
	Settings map[string]any `json:"settings"`
	Order    struct {
		Batch    string    `json:"batch"`
		Payments []RulePay `json:"payments"`
	} `json:"order"`
	Refund RulePay `json:"refund"`
	// Payment / TipAfter: a tips case (a payment taken with its tip, or a
	// tip set afterwards on it).
	Payment  RulePay         `json:"payment"`
	TipAfter bool            `json:"tip_after"`
	Pi       json.RawMessage `json:"pi"`
}

// LoadRules reads the table from path.
func LoadRules(t *testing.T, path string) RuleTable {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tab RuleTable
	if err := json.Unmarshal(b, &tab); err != nil {
		t.Fatal(err)
	}
	return tab
}

// ConfigFor is the terminal config main would send for the table's people
// (each one's effective_limits as main writes it: a manager / an unlimited
// person = both null) under the table's settings + extra.
func (tab RuleTable) ConfigFor(t *testing.T, extra map[string]any) *terminal.Config {
	t.Helper()
	set := map[string]any{}
	for k, v := range tab.Settings {
		set[k] = v
	}
	for k, v := range extra {
		set[k] = v
	}
	var staff []map[string]any
	for name, p := range tab.People {
		if p.Absent {
			continue
		}
		eff := json.RawMessage(`{"refund_cents":null,"discount_pct":null}`)
		if len(p.Effective) > 0 && string(p.Effective) != "null" {
			eff = p.Effective
		}
		staff = append(staff, map[string]any{"id": name, "first_name": name, "role": p.Role, "effective_limits": eff})
	}
	body, _ := json.Marshal(map[string]any{"version": 1, "settings": set, "staff": staff})
	c, err := terminal.ParseConfig(body)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Staff is a person of the table by name.
func (tab RuleTable) Staff(c *terminal.Config, name string) terminal.Staff {
	if s, ok := c.Find(name); ok {
		return s
	}
	return terminal.Staff{ID: name, Role: tab.People[name].Role}
}

// Answer is a rule's answer in the table's words.
func Answer(need *terminal.ManagerRequired, err error) string {
	switch {
	case err != nil && errors.Is(err, terminal.ErrRefundTooBig):
		return `{"refused":"refund_too_big"}`
	case err != nil && errors.Is(err, terminal.ErrNotPayable):
		return `{"refused":"not_payable"}`
	case err != nil && errors.Is(err, terminal.ErrTipCardOnly):
		return `{"refused":"tip_card_only"}`
	case err != nil && errors.Is(err, terminal.ErrNotTippable):
		return `{"refused":"not_tippable"}`
	case err != nil:
		return "error: " + err.Error()
	case need != nil:
		return fmt.Sprintf(`{"limit":%d,"manager":%q,"requested":%d}`, need.Limit, need.Action, need.Requested)
	}
	return `"allow"`
}

// Want is the case's expected answer, normalised like Answer's.
func Want(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if m, ok := v.(map[string]any); ok {
		if _, mgr := m["manager"]; mgr {
			for _, k := range []string{"limit", "requested"} {
				if _, ok := m[k]; !ok {
					m[k] = 0
				}
			}
		}
		v = m
	}
	b, _ := json.Marshal(v)
	return string(b)
}
