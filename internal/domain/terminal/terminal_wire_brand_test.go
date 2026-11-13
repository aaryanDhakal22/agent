package terminal

import "testing"

// 2.5.5 (contract §2.10ag): a card payment's brand rides to main on the
// payment's wire shape; a payment without one sends none.
func TestWireCarriesTheBrand(t *testing.T) {
	p := Payment{ID: "p1", Tender: TenderCard, AmountCents: 1200, Last4: "4242", Brand: BrandVisa}
	if w := p.Wire(); w["brand"] != "visa" || w["last4"] != "4242" {
		t.Fatalf("wire: %+v", w)
	}
	p.Brand = ""
	if _, ok := p.Wire()["brand"]; ok {
		t.Fatal("no brand: none sent")
	}
}
