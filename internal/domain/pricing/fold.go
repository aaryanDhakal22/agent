package pricing

import "strings"

// Mods never stack (the owner, 2026-11-14, #20; agent contract §2.10an):
// every pick is its own mod line. The same option picked twice reads twice
// ("Pepperoni", "Pepperoni"), never "2x Pepperoni"; a qualifier ("Lite",
// "Extra") joins the next pick of its group: "Lite Fried Onions".

// QualifierNames are the option names that qualify the next pick of their
// group (SoftTouch's "Lite" / "Extra" buttons on the sub, sandwich, gyro,
// wrap and falafel screens), compared trimmed and in any case.
var QualifierNames = []string{"lite", "light", "extra", "xtra", "easy", "less", "heavy"}

// IsQualifier reports whether an option named name is a qualifier.
func IsQualifier(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, q := range QualifierNames {
		if n == q {
			return true
		}
	}
	return false
}

// ModLines are an item's options as order line children: one per mod line,
// in order. A qualifier the engine joined to the next pick (Attached) is
// left out and its price goes to that pick's line (whose Label already
// reads "Lite Fried Onions"). Nothing folds: Count is always 1.
func ModLines(opts []ResolvedOption) []ResolvedOption {
	out := make([]ResolvedOption, 0, len(opts))
	var carry int64
	for _, o := range opts {
		o.Count = 1
		if o.Attached {
			if !o.RemovedDefault && !o.Included {
				carry += o.PriceCents
			}
			continue
		}
		if carry != 0 && !o.RemovedDefault && !o.Included {
			o.PriceCents += carry
		}
		carry = 0
		out = append(out, o)
	}
	return out
}
