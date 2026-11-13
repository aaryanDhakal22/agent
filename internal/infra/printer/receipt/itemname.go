package receipt

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
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
	size, name = inchParts(size, name)
	return titleWords(strings.Join(nonEmpty(size, name), " "))
}

// inchParts is the size (the vendor's placeholder dropped) and the name with
// an inch size marked: 8 Sub -> 8" Sub, a 12 Medium pizza -> 12" Medium.
func inchParts(size, name string) (string, string) {
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
	return size, name
}

// kitchenName is a station ticket's item name (the owner, 2.5.2): Title Case
// with the inch mark; a store order keeps its size in brackets on the name
// line ("Pepperoni Pizza (Large)", held together when it wraps), an online
// vendor order's size goes to its own "- " line (kitchenSizeLine).
func kitchenName(it order.LineItem, online bool) string {
	size, name := inchParts(it.Size, it.Name)
	name = titleWords(name)
	if size == "" || online {
		return name
	}
	return name + " (" + strings.ReplaceAll(titleWords(size), " ", "\x1f") + ")" // \x1f: a space wordWrap keeps
}

// kitchenSizeLine is an online order's size under its name ("6 Piece"; the
// vendor's spaced slash "6 / Piece" reads as a space), "" for none.
func kitchenSizeLine(it order.LineItem, online bool) string {
	if !online {
		return ""
	}
	size, _ := inchParts(it.Size, it.Name)
	return titleWords(strings.Join(strings.Fields(strings.ReplaceAll(size, " / ", " ")), " "))
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

// acronyms stay in capitals when a name comes all in capitals.
var acronyms = map[string]bool{"BBQ": true, "BLT": true, "BLTS": true, "PBJ": true, "NY": true, "XL": true, "XXL": true,
	"PC": true, "PCS": true, "USA": true, "GF": true, "BCR": true, "CBR": true}

// titleWords capitalises the first letter of each word (a unit such as oz
// aside), leaving the rest as the menu spells it; a name that comes all in
// capitals (a vendor's "WHOLE WINGS COMBO") reads as Title Case, its
// acronyms kept (BBQ, BLT).
func titleWords(s string) string {
	if s != strings.ToLower(s) && s == strings.ToUpper(s) {
		f := strings.Split(s, " ")
		for i, w := range f {
			if !acronyms[strings.Trim(w, "(),.\x1f")] && !strings.Contains(w, "\x1f") {
				f[i] = strings.ToLower(w)
			}
		}
		s = strings.Join(f, " ")
	}
	words := strings.Split(s, " ")
	for i, w := range words {
		r, n := utf8.DecodeRuneInString(w)
		if n > 0 && unicode.IsLower(r) && !isUnit(w) { // "20 oz" keeps its unit as written
			words[i] = string(unicode.ToUpper(r)) + w[n:]
		}
	}
	return strings.Join(words, " ")
}
