package order

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeReceiptPrefs_MissingKeysAreDefaults(t *testing.T) {
	for _, raw := range []string{"", "{}", `{"footer_text":null}`} {
		p, err := DecodeReceiptPrefs([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if p != DefaultReceiptPrefs() {
			t.Errorf("%q: got %+v want defaults", raw, p)
		}
	}
	// A partial object keeps the keys it has; the bools it lacks stay on.
	p, err := DecodeReceiptPrefs([]byte(`{"kitchen_item_size":"xlarge","show_phone":false,"footer_text":"","copies":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.KitchenItemSize != SizeXLarge || p.ShowPhone || p.Copies != 2 || p.FooterText == nil || *p.FooterText != "" {
		t.Errorf("explicit keys lost: %+v", p)
	}
	if p.KitchenNumberSize != SizeXLarge || p.FooterSize != SizeXLarge || !p.ShowStoreAddress || !p.ExtraPaper {
		t.Errorf("missing keys should be defaults: %+v", p)
	}
	// Empty strings / zero copies (an older or sloppy writer) normalize.
	p, _ = DecodeReceiptPrefs([]byte(`{"kitchen_item_size":"","copies":0}`))
	if p.KitchenItemSize != SizeLarge || p.Copies != 1 {
		t.Errorf("got %+v", p)
	}
	if _, err := DecodeReceiptPrefs([]byte(`{"copies":"two"}`)); err == nil {
		t.Error("bad JSON is an error")
	}
}

func TestReceiptPrefs_JSONRoundTrip(t *testing.T) {
	f := "THANK YOU"
	in := DefaultReceiptPrefs()
	in.FooterText, in.ExtraPaper, in.KitchenNumberSize = &f, false, SizeHuge
	b, _ := json.Marshal(in)
	for _, key := range []string{`"kitchen_item_size"`, `"footer_text":"THANK YOU"`, `"extra_paper":false`, `"show_phone":true`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON missing %s: %s", key, b)
		}
	}
	out, err := DecodeReceiptPrefs(b)
	if err != nil || out.FooterText == nil || *out.FooterText != f || out.ExtraPaper || out.KitchenNumberSize != SizeHuge {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	if b, _ := json.Marshal(DefaultReceiptPrefs()); !strings.Contains(string(b), `"footer_text":null`) {
		t.Errorf("the default footer is null: %s", b)
	}
}

func TestReceiptPrefs_Validate(t *testing.T) {
	if err := DefaultReceiptPrefs().Validate(); err != nil {
		t.Fatal(err)
	}
	str := func(s string) *string { return &s }
	bad := map[string]func(*ReceiptPrefs){
		"item huge":        func(p *ReceiptPrefs) { p.KitchenItemSize = SizeHuge },
		"item unknown":     func(p *ReceiptPrefs) { p.KitchenItemSize = "tiny" },
		"number normal":    func(p *ReceiptPrefs) { p.KitchenNumberSize = SizeNormal },
		"footer size huge": func(p *ReceiptPrefs) { p.FooterSize = SizeHuge },
		"copies 0":         func(p *ReceiptPrefs) { p.Copies = 0 },
		"copies 3":         func(p *ReceiptPrefs) { p.Copies = 3 },
		"footer too long":  func(p *ReceiptPrefs) { p.FooterText = str(strings.Repeat("A", MaxFooterLen+1)) },
		"footer accent":    func(p *ReceiptPrefs) { p.FooterText = str("MERCI BEAUCOUP CAFÉ") },
		"footer emoji":     func(p *ReceiptPrefs) { p.FooterText = str("THANKS 🍕") },
		"footer newline":   func(p *ReceiptPrefs) { p.FooterText = str("A\nB") },
	}
	for name, edit := range bad {
		p := DefaultReceiptPrefs()
		edit(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
	good := DefaultReceiptPrefs()
	good.KitchenItemSize, good.KitchenNumberSize, good.FooterSize, good.Copies = SizeNormal, SizeHuge, SizeNormal, 2
	good.FooterText = str(strings.Repeat("A", MaxFooterLen))
	if err := good.Validate(); err != nil {
		t.Errorf("valid prefs rejected: %v", err)
	}
	good.FooterText = str("")
	if err := good.Validate(); err != nil {
		t.Errorf("an empty footer (= none) is valid: %v", err)
	}
}

func TestReceiptPrefs_Footer(t *testing.T) {
	p := DefaultReceiptPrefs()
	if p.Footer("") != "ONLINE ORDER" || p.Footer("THANKS") != "THANKS" {
		t.Error("nil setting falls back to the location footer, then ONLINE ORDER")
	}
	none, mine := "", "MINE"
	p.FooterText = &none
	if p.Footer("THANKS") != "" {
		t.Error("an empty setting means no footer")
	}
	p.FooterText = &mine
	if p.Footer("THANKS") != "MINE" {
		t.Error("the setting wins")
	}
}

// bold_choice_words (2.5.2): defaults fries / salad / soda, a JSON list that round-trips, any-case
// matching, ≤ 10 words of ≤ 20 plain characters, a missing key keeps the store's.
func TestBoldChoiceWords(t *testing.T) {
	d := DefaultReceiptPrefs()
	if got := d.BoldChoiceWords.Words(); len(got) != 3 || got[0] != "fries" || got[2] != "soda" {
		t.Fatalf("defaults %v", got)
	}
	if !d.BoldChoiceWords.Matches("Side of FRIES") || !d.BoldChoiceWords.Matches("House Salad") || d.BoldChoiceWords.Matches("Pepperoni") {
		t.Error("matching")
	}
	p, err := DecodeReceiptPrefs([]byte(`{"bold_choice_words":[" Ranch ","","cheese"]}`))
	if err != nil || len(p.BoldChoiceWords.Words()) != 2 || !p.BoldChoiceWords.Matches("extra RANCH") {
		t.Fatalf("decode: %v %v", p.BoldChoiceWords.Words(), err)
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"bold_choice_words":["Ranch","cheese"]`) {
		t.Errorf("encode: %s", b)
	}
	if p, _ := DecodeReceiptPrefs([]byte(`{"copies":2}`)); p.BoldChoiceWords != d.BoldChoiceWords {
		t.Error("a missing key keeps the defaults")
	}
	if p, _ := DecodeReceiptPrefs([]byte(`{"bold_choice_words":[]}`)); len(p.BoldChoiceWords.Words()) != 0 || p.Validate() != nil {
		t.Error("an empty list is allowed: nothing bold")
	}
	many := d
	many.BoldChoiceWords = NewWordList("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k")
	if many.Validate() == nil {
		t.Error("11 words")
	}
	long := d
	long.BoldChoiceWords = NewWordList(strings.Repeat("x", 21))
	if long.Validate() == nil {
		t.Error("21 characters")
	}
	accent := d
	accent.BoldChoiceWords = NewWordList("café")
	if accent.Validate() == nil {
		t.Error("accents")
	}
}
