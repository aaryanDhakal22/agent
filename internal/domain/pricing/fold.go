package pricing

import "fmt"

// FoldRepeats folds an item's repeated options (a double / triple topping:
// the same option picked again on the same portion + mode, which only an
// allow_multiple group lets through) into one, in the first one's place:
// Count = how many, Label = "2x Pepperoni (1st half)". Options that differ
// in price or in being included in a bundle stay apart; a NO / a default
// left off never folds. Order lines are built from the result: one child
// "2x Pepperoni" at twice the price, so a ticket prints one line. Plain
// ASCII "x" (the quote's mods say "2×"): the printers' code page (PC437) has
// no multiplication sign, and older Pis print the name as it is.
func FoldRepeats(opts []ResolvedOption) []ResolvedOption {
	type key struct {
		name, portion, mode string
		price               int64
		included            bool
	}
	out := make([]ResolvedOption, 0, len(opts))
	at := map[key]int{}
	for _, o := range opts {
		if o.Count < 1 {
			o.Count = 1
		}
		if o.RemovedDefault || o.Mode == ModeSpecialty {
			out = append(out, o)
			continue
		}
		k := key{o.Name, o.Portion, o.Mode, o.PriceCents, o.Included}
		if i, ok := at[k]; ok {
			out[i].Count += o.Count
			out[i].Label = fmt.Sprintf("%dx %s", out[i].Count, OptionLabel(o.Name, o.Portion, o.Mode))
			continue
		}
		at[k] = len(out)
		out = append(out, o)
	}
	return out
}
