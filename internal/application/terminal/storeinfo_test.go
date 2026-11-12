package terminal

import (
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/storeconfig"
)

func TestStoreInfo(t *testing.T) {
	sc := storeconfig.StoreContext{Location: storeconfig.LocationContext{ID: "l", Name: "Lombardi's", Phone: "4105550100",
		AddressLine1: "931 Taylor Ave", AddressLine2: "Towson, MD 21286",
		Config: map[string]any{"website": "www.lombardis.com", "receipt_footer": "Order online!"}}}
	got := StoreInfo(sc, order.DefaultReceiptPrefs())
	if got["address"] != "931 Taylor Ave, Towson, MD 21286" || got["phone"] != "4105550100" || got["website"] != "www.lombardis.com" ||
		got["footer"] != "Order online!" {
		t.Fatalf("%v", got)
	}
	if StoreInfo(storeconfig.StoreContext{}, order.DefaultReceiptPrefs()) != nil {
		t.Fatal("no store linked: nothing")
	}
}
