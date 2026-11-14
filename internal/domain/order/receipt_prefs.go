package order

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Text sizes of the receipt settings, smallest first. Not every setting takes
// every size (see ReceiptPrefs).
const (
	SizeSmall  = "small" // the driver report only (font B)
	SizeNormal = "normal"
	SizeLarge  = "large"
	SizeXLarge = "xlarge"
	SizeHuge   = "huge"
)

// Customer receipt formats (2.4.0): expanded = every modifier with its
// price and a Sum per item (the receipt as it always was); condensed = one
// line per item priced with all its modifiers, and only its first modifier.
const (
	FormatExpanded  = "expanded"
	FormatCondensed = "condensed"
)

// MaxFooterLen is the longest receipt footer (one line at the base font).
const MaxFooterLen = 32

// ReceiptPrefs are the receipt settings the store manager changes from the
// tablet. The defaults print exactly what the agent printed before these
// settings existed. Stored as JSON in settings.receipt_prefs; a key missing
// from the stored object means its default (decode with DecodeReceiptPrefs).
type ReceiptPrefs struct {
	// KitchenItemSize: the kitchen ticket's own items — normal | large | xlarge.
	KitchenItemSize string `json:"kitchen_item_size"`
	// KitchenNumberSize: the kitchen number on top of the kitchen ticket —
	// large | xlarge | huge.
	KitchenNumberSize string `json:"kitchen_number_size"`
	// FooterText: nil = the location's receipt_footer, else "ONLINE ORDER";
	// "" = no footer line. At most MaxFooterLen printable ASCII characters.
	FooterText *string `json:"footer_text"`
	// FooterSize: normal | large | xlarge.
	FooterSize string `json:"footer_size"`
	// Copies of the customer receipt (1 or 2); kitchen tickets print once.
	Copies int `json:"copies"`
	// ShowStoreAddress: the location's name + address at the top of the
	// customer receipt.
	ShowStoreAddress bool `json:"show_store_address"`
	// ShowPhone: the customer's phone line on the customer receipt.
	ShowPhone bool `json:"show_phone"`
	// ExtraPaper: feed past the footer and cut twice (today's ending); off =
	// a short feed and one cut.
	ExtraPaper bool `json:"extra_paper"`
	// DriverReportSize: the driver-report slip's one text size — small (font
	// B) | normal (font A, the default) | large (font A double height, the
	// size before this setting).
	DriverReportSize string `json:"driver_report_size"`
	// Spacing: extra paper (printer dots) on kitchen tickets and customer
	// receipts between an item and its first modifier, between modifiers,
	// and between one item (with or without modifiers) and the next. 0 = none
	// (the look before this setting).
	Spacing LineSpacing `json:"spacing"`
	// Format (2.4.0): the customer receipt's item list - expanded |
	// condensed (reprints too; kitchen tickets never change).
	Format string `json:"format"`
	// StrikeImages (kitchen change tickets): a removed line prints "(VOID)"
	// and the item struck through as a small picture; off = "(VOID)" in
	// reverse and the item as text, for a printer that can't print pictures.
	StrikeImages bool `json:"strike_images"`
	// BoldChoiceWords (2.5.2, the owner's stopgap until the storefront): a
	// choice printed on a customer slip whose name holds one of these words
	// (any case) prints bold. At most MaxBoldWords words of MaxBoldWordLen
	// plain characters.
	BoldChoiceWords WordList `json:"bold_choice_words"`
	// HandoverChoiceWords (agent 2.5.7, the owner 2026-11-13: the delivery
	// ticket shows "only the ones the driver hands over": drinks, sides,
	// sauces, dressings, never a cooking choice such as "Well Done"): a $0
	// choice on a customer slip prints when its name holds one of these as
	// a whole word (any case) and it isn't a NO, or our menu stamped it
	// show_on_driver_ticket. The online vendor's choices are never stamped,
	// so for them these words decide. At most MaxHandoverWords words of
	// MaxBoldWordLen plain characters.
	HandoverChoiceWords WordList `json:"handover_choice_words"`
	// StyleOnline / StyleStore (2.5.2): the customer slips' layout for online
	// vendor orders (source online) and for the store's own (counter, web,
	// call center): softtouch (SoftTouch's slip, the default) | classic (the
	// receipt the Pi printed before it).
	StyleOnline string `json:"style_online"`
	StyleStore  string `json:"style_store"`

	// DrinkMarks (the owner 2026-11-14, contract §2.1k: SoftTouch's marks,
	// "drivers scan a long delivery ticket for them to know what to carry"):
	// on, every line stamped a drink (LineItem.Drink) ends in DrinkMark on a
	// delivery's customer slips. Off = no marks; the stamps stay.
	DrinkMarks bool `json:"drink_marks"`
	// DrinkCategories: an item of a counter-menu category whose name is one
	// of these (any case, punctuation and a "(Counter)" ending ignored) is a
	// drink. At most MaxDrinkWords names of MaxBoldWordLen plain characters.
	DrinkCategories WordList `json:"drink_categories"`
	// DrinkGroups: a choice of a modifier group whose name holds one of these
	// as a whole word (any case) is a drink ("Whole Wings Soda (Counter)").
	// At most MaxDrinkWords words of MaxBoldWordLen plain characters.
	DrinkGroups WordList `json:"drink_groups"`
	// DrinkSkipWords: an option of a drink group whose name holds one of
	// these as a whole word is not a drink (Lombardi's "Whole Wings Soda
	// (Counter)" also sells "Extra BBq Sauce", "BBQ on Tender", "SP & K on
	// Fries"). At most MaxDrinkWords words of MaxBoldWordLen plain characters.
	DrinkSkipWords WordList `json:"drink_skip_words"`

	// TakeoutTwoCopies is not a tablet setting: main's terminal setting
	// takeout_two_copies (Store › Settings › Receipts and printing, the
	// owner 2026-11-13), set on the prefs at print time from the terminal
	// config (cmd/main.go). On: a pickup's customer receipt prints at least
	// two copies (receipt.CustomerCopies). Never stored, never sent.
	TakeoutTwoCopies bool `json:"-"`
}

// The receipt styles.
const (
	StyleSoftTouch = "softtouch"
	StyleClassic   = "classic"
)

// StyleFor is the slip style for an order from source.
func (p ReceiptPrefs) StyleFor(source string) string {
	s := p.StyleStore
	if source == SourceOnline {
		s = p.StyleOnline
	}
	if s == StyleClassic {
		return StyleClassic
	}
	return StyleSoftTouch
}

// MaxBoldWords / MaxBoldWordLen bound ReceiptPrefs.BoldChoiceWords;
// MaxHandoverWords (and MaxBoldWordLen) ReceiptPrefs.HandoverChoiceWords;
// MaxDrinkWords each of DrinkCategories / DrinkGroups.
const (
	MaxBoldWords     = 10
	MaxBoldWordLen   = 20
	MaxHandoverWords = 80
	MaxDrinkWords    = 20
)

// DrinkMark is what ends a drink's line on a delivery's customer slips
// (SoftTouch's, the owner 2026-11-14: exactly five).
const DrinkMark = "#####"

// DefaultDrinkCategories / DefaultDrinkGroups are the drink rule a Pi starts
// with (Lombardi's counter menu, 2026-11-14: the "Beverages" category, the
// "Whole Wings Soda (Counter)" and "Milk Shake Mods (Counter)" groups).
var (
	DefaultDrinkCategories = []string{"beverages", "beverage", "drinks", "drink", "soda", "sodas"}
	DefaultDrinkGroups     = []string{"soda", "sodas", "drink", "drinks", "beverage", "beverages", "juice", "juices",
		"shake", "shakes", "slushie", "slushies", "slurpy", "lemonade"}
	DefaultDrinkSkipWords = []string{"sauce", "souce", "sause", "extra", "tender", "tenders", "fries", "all over",
		"on side", "dressing", "ranch"}
)

// DrinkRule is the part of the receipt settings the Pi stamps drinks with
// (comparable: a cache key).
type DrinkRule struct{ Categories, Groups, Skip WordList }

// DrinkRule is these settings' drink rule.
func (p ReceiptPrefs) DrinkRule() DrinkRule {
	return DrinkRule{Categories: p.DrinkCategories, Groups: p.DrinkGroups, Skip: p.DrinkSkipWords}
}

// CategoryIsDrink: the category's name is one of the drink categories
// (DrinkNameKey of both, a "(Counter)"-style ending left off the name).
func (r DrinkRule) CategoryIsDrink(name string) bool {
	k := DrinkNameKey(parenEnd.ReplaceAllString(name, ""))
	if k == "" {
		return false
	}
	for _, w := range r.Categories.Words() {
		if DrinkNameKey(w) == k {
			return true
		}
	}
	return false
}

// GroupIsDrink: the modifier group's name holds a drink group word as a
// whole word.
func (r DrinkRule) GroupIsDrink(name string) bool { return r.Groups.MatchesWord(name) }

// OptionIsDrink: an option of a drink group is a drink unless its name holds
// a skip word as a whole word.
func (r DrinkRule) OptionIsDrink(name string) bool { return !r.Skip.MatchesWord(name) }

var parenEnd = regexp.MustCompile(`\s*\([^()]*\)\s*$`)

// DrinkNameKey is a name compared for the drink rule: lower case, letters
// and digits only ("2 Lt. Diet Pepsi" = "2ltdietpepsi").
func DrinkNameKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 0x7f && unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// DefaultHandoverWords are the hand-over words a Pi starts with (Lombardi's
// online vendor and counter names, 2026-11-13): every drink name the owner's
// menu uses (cans, 2 liters, fountain half & half / lemonade / triple mix,
// shakes, ice; the owner: "drink choices are very important"),
// sides, dips and dressings, desserts. Sauces as such are left out ("Add
// Ketchup Sauce" goes on a sub): our menu flags the sauce cups
// (show_on_driver_ticket), the vendor names them "... Dip".
var DefaultHandoverWords = []string{
	"can", "cans", "soda", "pepsi", "coke", "sprite", "ginger ale", "ice tea", "iced tea", "tea", "dr pepper",
	"root beer", "mtn dew", "mountain dew", "lemonade", "fruit punch", "punch", "water", "juice", "2 liter", "liter",
	"bottle", "drink", "half/half", "half & half", "half and half", "triple mix", "shake", "lassi", "lasi", "slushie",
	"ice", "orange", "grape",
	"fries", "side", "salad", "chips", "celery", "rice", "naan", "roti",
	"dip", "dressing", "marinara", "chutney",
	"dessert", "cake", "cheesecake", "cannoli", "brownie", "cookie",
}

// WordList is a short list of words kept comparable (ReceiptPrefs is compared
// with ==): JSON is an array of strings.
type WordList struct{ joined string }

// NewWordList keeps the words trimmed, empty ones dropped.
func NewWordList(words ...string) WordList {
	var keep []string
	for _, w := range words {
		if w = strings.TrimSpace(w); w != "" {
			keep = append(keep, w)
		}
	}
	return WordList{joined: strings.Join(keep, "\n")}
}

// Words is the list (never nil).
func (l WordList) Words() []string {
	if l.joined == "" {
		return []string{}
	}
	return strings.Split(l.joined, "\n")
}

// MatchesWord: name holds one of the words as a whole word (or words), any
// case: "can" is in "Can of Ice Tea" and "W Can Pepsi", never in "American".
func (l WordList) MatchesWord(name string) bool {
	if l.joined == "" {
		return false
	}
	n := strings.ToLower(name)
	for _, w := range l.Words() {
		w = strings.ToLower(w)
		for from := 0; from < len(n); {
			i := strings.Index(n[from:], w)
			if i < 0 {
				break
			}
			i += from
			end := i + len(w)
			if (i == 0 || !wordByte(n[i-1])) && (end == len(n) || !wordByte(n[end])) {
				return true
			}
			from = i + 1
		}
	}
	return false
}

func wordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c >= 0x80
}

// Matches: name holds one of the words, any case.
func (l WordList) Matches(name string) bool {
	if l.joined == "" {
		return false
	}
	n := strings.ToLower(name)
	for _, w := range l.Words() {
		if strings.Contains(n, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

func (l WordList) MarshalJSON() ([]byte, error) { return json.Marshal(l.Words()) }

func (l *WordList) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil // keeps what it was (decoded over the defaults)
	}
	var ws []string
	if err := json.Unmarshal(b, &ws); err != nil {
		return fmt.Errorf("a word list must be a list of words")
	}
	for _, w := range ws {
		if strings.Contains(w, "\n") {
			return fmt.Errorf("a word list: a word can't hold a line break")
		}
	}
	if len(ws) > 4*MaxHandoverWords { // Validate gives the real limit per list; this only bounds the work
		return fmt.Errorf("a word list: at most %d words", MaxHandoverWords)
	}
	*l = NewWordList(ws...)
	return nil
}

// LineSpacing is ReceiptPrefs.Spacing, in printer dots (0..MaxSpacingDots).
type LineSpacing struct {
	ItemMod  int `json:"item_mod"`
	ModMod   int `json:"mod_mod"`
	ItemItem int `json:"item_item"`
}

// MaxSpacingDots caps each line spacing (48 dots = 6 mm at 203 dpi).
const MaxSpacingDots = 48

// DefaultReceiptPrefs are the settings that print like the agent always did.
func DefaultReceiptPrefs() ReceiptPrefs {
	return ReceiptPrefs{
		KitchenItemSize:     SizeLarge,
		KitchenNumberSize:   SizeXLarge,
		FooterSize:          SizeXLarge,
		Copies:              1,
		ShowStoreAddress:    true,
		ShowPhone:           true,
		ExtraPaper:          true,
		DriverReportSize:    SizeNormal,
		Format:              FormatExpanded,
		StrikeImages:        true,
		BoldChoiceWords:     NewWordList("fries", "salad", "soda"),
		HandoverChoiceWords: NewWordList(DefaultHandoverWords...),
		StyleOnline:         StyleSoftTouch,
		StyleStore:          StyleSoftTouch,
		DrinkMarks:          true,
		DrinkCategories:     NewWordList(DefaultDrinkCategories...),
		DrinkGroups:         NewWordList(DefaultDrinkGroups...),
		DrinkSkipWords:      NewWordList(DefaultDrinkSkipWords...),
	}
}

// DecodeReceiptPrefs reads stored (or sent) prefs: the JSON is decoded over
// the defaults, so a key it lacks keeps its default ("{}" = all defaults).
// Empty input is "{}".
func DecodeReceiptPrefs(raw []byte) (ReceiptPrefs, error) {
	p := DefaultReceiptPrefs()
	if len(raw) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return DefaultReceiptPrefs(), err
	}
	return p.Normalize(), nil
}

// Normalize fills empty sizes and a zero copy count with their defaults
// (the booleans can't be told apart from "off" — DecodeReceiptPrefs handles
// those by decoding over the defaults).
func (p ReceiptPrefs) Normalize() ReceiptPrefs {
	d := DefaultReceiptPrefs()
	if p.KitchenItemSize == "" {
		p.KitchenItemSize = d.KitchenItemSize
	}
	if p.KitchenNumberSize == "" {
		p.KitchenNumberSize = d.KitchenNumberSize
	}
	if p.FooterSize == "" {
		p.FooterSize = d.FooterSize
	}
	if p.Copies == 0 {
		p.Copies = d.Copies
	}
	if p.DriverReportSize == "" {
		p.DriverReportSize = d.DriverReportSize
	}
	if p.StyleOnline == "" {
		p.StyleOnline = d.StyleOnline
	}
	if p.StyleStore == "" {
		p.StyleStore = d.StyleStore
	}
	if p.Format == "" {
		p.Format = d.Format
	}
	return p
}

// validWords checks one word list setting: at most max words of
// MaxBoldWordLen plain characters.
func validWords(key string, l WordList, max int) error {
	ws := l.Words()
	if len(ws) > max {
		return fmt.Errorf("%s: at most %d words", key, max)
	}
	for _, w := range ws {
		if len(w) > MaxBoldWordLen {
			return fmt.Errorf("%s: a word is at most %d characters", key, MaxBoldWordLen)
		}
		for i := 0; i < len(w); i++ {
			if c := w[i]; c < 0x20 || c > 0x7e {
				return fmt.Errorf("%s may only use plain letters, digits, spaces and punctuation", key)
			}
		}
	}
	return nil
}

// Validate returns a plain reason when a setting is out of range.
func (p ReceiptPrefs) Validate() error {
	if !oneOf(p.KitchenItemSize, SizeNormal, SizeLarge, SizeXLarge) {
		return fmt.Errorf("kitchen_item_size must be normal, large or xlarge")
	}
	if !oneOf(p.KitchenNumberSize, SizeLarge, SizeXLarge, SizeHuge) {
		return fmt.Errorf("kitchen_number_size must be large, xlarge or huge")
	}
	if !oneOf(p.FooterSize, SizeNormal, SizeLarge, SizeXLarge) {
		return fmt.Errorf("footer_size must be normal, large or xlarge")
	}
	if p.Copies < 1 || p.Copies > 2 {
		return fmt.Errorf("copies must be 1 or 2")
	}
	if !oneOf(p.DriverReportSize, SizeSmall, SizeNormal, SizeLarge) {
		return fmt.Errorf("driver_report_size must be small, normal or large")
	}
	if !oneOf(p.Format, FormatExpanded, FormatCondensed) {
		return fmt.Errorf("format must be expanded or condensed")
	}
	for _, v := range []int{p.Spacing.ItemMod, p.Spacing.ModMod, p.Spacing.ItemItem} {
		if v < 0 || v > MaxSpacingDots {
			return fmt.Errorf("spacing must be 0-%d dots", MaxSpacingDots)
		}
	}
	if !oneOf(p.StyleOnline, StyleSoftTouch, StyleClassic) || !oneOf(p.StyleStore, StyleSoftTouch, StyleClassic) {
		return fmt.Errorf("style_online and style_store must be softtouch or classic")
	}
	if err := validWords("bold_choice_words", p.BoldChoiceWords, MaxBoldWords); err != nil {
		return err
	}
	if err := validWords("handover_choice_words", p.HandoverChoiceWords, MaxHandoverWords); err != nil {
		return err
	}
	if err := validWords("drink_categories", p.DrinkCategories, MaxDrinkWords); err != nil {
		return err
	}
	if err := validWords("drink_groups", p.DrinkGroups, MaxDrinkWords); err != nil {
		return err
	}
	if err := validWords("drink_skip_words", p.DrinkSkipWords, MaxDrinkWords); err != nil {
		return err
	}
	if f := p.FooterText; f != nil {
		if len(*f) > MaxFooterLen {
			return fmt.Errorf("footer_text must be at most %d characters", MaxFooterLen)
		}
		for i := 0; i < len(*f); i++ {
			if c := (*f)[i]; c < 0x20 || c > 0x7e {
				return errors.New("footer_text may only use plain letters, digits, spaces and punctuation (no accents or emoji)")
			}
		}
	}
	return nil
}

// Footer is the footer line that prints: the setting, else the location's
// receipt_footer, else "ONLINE ORDER". "" = no footer line.
func (p ReceiptPrefs) Footer(locationFooter string) string {
	if p.FooterText != nil {
		return *p.FooterText
	}
	if locationFooter != "" {
		return locationFooter
	}
	return "ONLINE ORDER"
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}
