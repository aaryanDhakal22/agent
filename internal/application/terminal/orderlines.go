package terminal

import (
	"encoding/json"
	"fmt"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// Order lines (batch A, the owner 2026-11-14; contract §2.10an): a mod's
// price set at the counter (#11) and priced notes (#19). Mods never stack
// and Quicc Add's printer live in the engine (pricing.ModLines,
// Line.PrinterID) and buildItems.

// ModPriceChange is one cart option rung at another price than the menu's:
// Line = the cart line, Pick = its index in the line's options.
type ModPriceChange struct {
	Line         int    `json:"line"`
	Pick         int    `json:"pick"`
	OptionID     string `json:"option_id"`
	Name         string `json:"name"`
	FromCents    int64  `json:"from_cents"`
	ToCents      int64  `json:"to_cents"`
	NeedsManager bool   `json:"needs_manager,omitempty"`
	approved     bool   // the stored cart had it already (PUT …/cart)
}

func (c ModPriceChange) key(itemID string) string {
	return fmt.Sprintf("%s\x1f%s\x1f%d", itemID, c.OptionID, c.ToCents)
}

// modPriceChanges are the cart options the terminal priced itself (a pick's
// price_cents, a NO aside) at another price than today's menu price at the
// line's size and half rule. in is the cart as the terminal sent it.
func modPriceChanges(ctx pricing.Context, in pricing.Cart) []ModPriceChange {
	var out []ModPriceChange
	for i, l := range in.Lines {
		if l.Custom || l.DealID != "" {
			continue
		}
		for k, o := range l.Options {
			if o.PriceCents == nil || o.Mode == pricing.ModeNo {
				continue
			}
			sel := l.Selection
			sel.Options = append([]pricing.Pick(nil), l.Options...)
			sel.Options[k].PriceCents = nil
			r := pricing.Resolve(ctx, sel, 0)
			for _, ro := range r.Options {
				if ro.Pick == k+1 && ro.PriceCents != *o.PriceCents {
					out = append(out, ModPriceChange{Line: i, Pick: k, OptionID: o.OptionID, Name: ro.Name, FromCents: ro.PriceCents, ToCents: *o.PriceCents})
				}
			}
		}
	}
	return out
}

// checkModPrices marks the changes a manager must cover for st (a driver
// lowering a mod) and answers that need; a change the stored cart already
// had (kept: key → count) was approved then.
func checkModPrices(cfg *terminal.Config, st terminal.Staff, cart pricing.Cart, changes []ModPriceChange, kept map[string]int) *terminal.ManagerRequired {
	var pairs [][2]int64
	for i := range changes {
		c := &changes[i]
		if c.Line < len(cart.Lines) {
			if k := c.key(cart.Lines[c.Line].ItemID); kept[k] > 0 {
				kept[k]--
				c.approved = true
				continue
			}
		}
		if terminal.ModPriceNeed(cfg, st, [][2]int64{{c.FromCents, c.ToCents}}) != nil {
			c.NeedsManager = true
		}
		pairs = append(pairs, [2]int64{c.FromCents, c.ToCents})
	}
	return terminal.ModPriceNeed(cfg, st, pairs)
}

// keptModPrices: the stored cart's option prices (item + option + price →
// count), so a PUT …/cart neither asks nor journals them again.
func keptModPrices(snap order.Snapshot) map[string]int {
	tm, ok := snap.SourceMeta["terminal"].(map[string]any)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(tm["cart"])
	if err != nil {
		return nil
	}
	var c Cart
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	out := map[string]int{}
	for _, l := range c.Lines {
		for _, o := range l.Options {
			if o.PriceCents != nil {
				out[ModPriceChange{OptionID: o.OptionID, ToCents: *o.PriceCents}.key(l.ItemID)]++
			}
		}
	}
	return out
}

// modPriceEntries journals every new mod price change.
func modPriceEntries(a Actor, st terminal.Staff, changes []ModPriceChange, approver *terminal.Staff, orderID string) []terminal.Entry {
	var out []terminal.Entry
	for _, c := range changes {
		if c.approved {
			continue
		}
		d := map[string]any{"line": c.Line, "pick": c.Pick, "option_id": c.OptionID, "name": c.Name, "from_cents": c.FromCents, "to_cents": c.ToCents}
		if c.NeedsManager && approver != nil {
			d["approved_by"] = approver.ID
		}
		out = append(out, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionModPriceChange, OrderID: orderID, Detail: detail(d)})
	}
	return out
}

// noteIssues: a priced note (a custom line with note_of) must follow its
// item: an earlier line that is neither custom nor a deal.
func noteIssues(c pricing.Cart) []pricing.Issue {
	var out []pricing.Issue
	for i, l := range c.Lines {
		if l.NoteOf == nil {
			continue
		}
		at := *l.NoteOf
		if !l.Custom || at < 0 || at >= i || c.Lines[at].Custom || c.Lines[at].DealID != "" {
			idx := i
			out = append(out, pricing.Issue{Code: pricing.IssueCustomLine, Message: "A priced note must follow its item.", Line: &idx})
		}
	}
	return out
}
