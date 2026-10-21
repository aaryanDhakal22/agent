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
