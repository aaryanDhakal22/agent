package order

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Text sizes of the receipt settings, smallest first. Not every setting takes
// every size (see ReceiptPrefs).
const (
	SizeNormal = "normal"
	SizeLarge  = "large"
	SizeXLarge = "xlarge"
	SizeHuge   = "huge"
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
}

// DefaultReceiptPrefs are the settings that print like the agent always did.
func DefaultReceiptPrefs() ReceiptPrefs {
	return ReceiptPrefs{
		KitchenItemSize:   SizeLarge,
		KitchenNumberSize: SizeXLarge,
		FooterSize:        SizeXLarge,
		Copies:            1,
		ShowStoreAddress:  true,
		ShowPhone:         true,
		ExtraPaper:        true,
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
	return p
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
