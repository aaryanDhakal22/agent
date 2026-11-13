package terminal

import (
	"strings"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/storeconfig"
)

// StoreInfo is the store's header / footer facts for the terminals'
// receipts (contract §2.10q): from store_config and the Pi's receipt footer.
func StoreInfo(sc storeconfig.StoreContext, prefs order.ReceiptPrefs) map[string]any {
	loc := sc.Location
	if loc.ID == "" && sc.StoreName() == "" {
		return nil
	}
	str := func(k string) string {
		if v, ok := loc.Config[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	addr := strings.TrimSpace(loc.AddressLine1)
	if l2 := strings.TrimSpace(loc.AddressLine2); l2 != "" {
		if addr != "" {
			addr += ", "
		}
		addr += l2
	}
	return map[string]any{"name": sc.StoreName(), "address": addr, "address_line1": strings.TrimSpace(loc.AddressLine1),
		"address_line2": strings.TrimSpace(loc.AddressLine2), "phone": strings.TrimSpace(loc.Phone), "website": str("website"),
		"footer": prefs.Footer(str("receipt_footer")),
		// a printed choice holding one of these words prints bold (receipt_prefs, 2.5.2)
		"bold_choice_words": prefs.BoldChoiceWords.Words()}
}
