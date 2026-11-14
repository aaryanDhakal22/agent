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
	count, size, name := countParts(size, name)
	size, name = inchParts(size, name)
	return strings.Join(nonEmpty(count, titleWords(strings.Join(nonEmpty(size, name), " "))), " ")
}

// pieceCount is a piece count in a size or a name: "6 pc", "6 Piece", "2 Pcs",
// "(4 pc)", "40pc", "20-pc".
var pieceCount = regexp.MustCompile(`(?i)\(?\b(\d+)\s*-?\s*(?:pcs?|pieces?)\b\.?\)?`)

// countParts is the count-first rule (the owner, 2026-11-14, #4; MENU-PLAN
// §9; the terminal's item_name.dart countParts, keep them in step): a COUNTED
// item (wings, tenders, nuggets, mozz sticks...) prints its count first, as
// "N pc" ("1   6 pc Party Wings"). The count comes from the size (the vendor's
// "6 / Piece" and "Serving (2 Piece)" too), else from the name; it leaves the
// size / name it came from. count is "" when there is none.
func countParts(size, name string) (count, restSize, restName string) {
	size = strings.Join(strings.Fields(strings.ReplaceAll(displaySize(size), " / ", " ")), " ")
	name = strings.TrimSpace(name)
	take := func(s string) (string, string) {
		m := pieceCount.FindStringSubmatchIndex(s)
		if m == nil {
			return "", s
		}
		return s[m[2]:m[3]] + " pc", strings.Join(strings.Fields(s[:m[0]]+" "+s[m[1]:]), " ")
	}
	if count, size = take(size); count == "" {
		count, name = take(name)
	}
	return count, size, name
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
//
// A counted item (2026-11-14, #4) starts with its count, held together:
// "6 pc Party Wings" (an online order's count too: it leaves the size line).
func kitchenName(it order.LineItem, online bool) string {
	count, size, name := countParts(it.Size, it.Name)
	size, name = inchParts(size, name)
	name = titleWords(name)
	if count != "" {
		name = strings.TrimSpace(strings.ReplaceAll(count, " ", "\x1f") + " " + name) // \x1f: a space wordWrap keeps
	}
	if size == "" || online {
		return name
	}
	return name + " (" + strings.ReplaceAll(titleWords(size), " ", "\x1f") + ")" // \x1f: a space wordWrap keeps
}

// kitchenSizeLine is an online order's size under its name ("Large"; the
// vendor's spaced slash "6 / Piece" reads as a space), "" for none. A piece
// count is not here: it leads the name (kitchenName).
func kitchenSizeLine(it order.LineItem, online bool) string {
	if !online {
		return ""
	}
	_, size, name := countParts(it.Size, it.Name)
	size, _ = inchParts(size, name)
	return titleWords(size)
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
