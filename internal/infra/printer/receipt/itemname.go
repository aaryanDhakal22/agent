package receipt

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The item's name on the customer slips (the owner, agent 2.5.2): the size
// then the name, each word capitalised and the rest as the menu spells it
// (BBQ stays BBQ), and an inch size marked with " (8 Sub → 8" Sub).

var (
	// inchKinds: items sized in inches.
	inchKinds = regexp.MustCompile(`(?i)\b(pizzas?|subs?|calzones?|strombolis?)\b`)
	// inchSizes: a size field starting with one of these numbers is inches.
	inchSizes = map[string]bool{"8": true, "10": true, "12": true, "14": true, "16": true, "18": true}
	// unitWords after a number make it a count or a volume, never inches.
	unitWords = map[string]bool{"oz": true, "ounce": true, "ounces": true, "liter": true, "liters": true, "litre": true,
		"litres": true, "l": true, "ml": true, "pc": true, "pcs": true, "piece": true, "pieces": true, "ct": true,
		"count": true, "pack": true, "lb": true, "lbs": true, "can": true, "cans": true, "bottle": true, "bottles": true}
)

func slipItemName(size, name string) string {
	size, name = strings.TrimSpace(displaySize(size)), strings.TrimSpace(name)
	if f := strings.Fields(size); len(f) > 0 && allDigits(f[0]) {
		next := ""
		if len(f) > 1 {
			next = f[1]
		} else if g := strings.Fields(name); len(g) > 0 {
			next = g[0]
		}
		if !isUnit(next) && (inchSizes[f[0]] || inchKinds.MatchString(size+" "+name)) {
			size = f[0] + `"` + strings.TrimPrefix(size, f[0])
		}
	} else if g := strings.Fields(name); size == "" && len(g) > 1 && allDigits(g[0]) && !isUnit(g[1]) && inchKinds.MatchString(name) {
		name = g[0] + `"` + strings.TrimPrefix(name, g[0])
	}
	return titleWords(strings.Join(nonEmpty(size, name), " "))
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isUnit(w string) bool {
	return unitWords[strings.ToLower(strings.Trim(w, ".,()"))]
}

// titleWords capitalises the first letter of each word (a unit such as oz
// aside), leaving the rest.
func titleWords(s string) string {
	words := strings.Split(s, " ")
	for i, w := range words {
		r, n := utf8.DecodeRuneInString(w)
		if n > 0 && unicode.IsLower(r) && !isUnit(w) { // "20 oz" keeps its unit as written
			words[i] = string(unicode.ToUpper(r)) + w[n:]
		}
	}
	return strings.Join(words, " ")
}
