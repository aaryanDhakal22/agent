package pricing

import (
	"encoding/json"
	"os"
	"testing"
)

// The included toppings mark their options Included (price kept, not
// charged): an order line prints them at $0.00 and a kept pick price stays
// the topping's own.
func TestIncludedToppingsResolve(t *testing.T) {
	raw, err := os.ReadFile("testdata/cases/69_included_never_included_option.json")
	if err != nil {
		t.Fatal(err)
	}
	var c pricingCase
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	r := Resolve(c.Context, c.Cart.Lines[0].Selection, 0)
	if r.UnitCents != 2099 || r.BaseCents != 1799 {
		t.Fatalf("unit %d base %d", r.UnitCents, r.BaseCents)
	}
	free := 0
	for _, o := range r.Options {
		switch {
		case o.Name == "Shrimp" && (o.Included || o.PriceCents != 300):
			t.Fatalf("shrimp: %+v", o)
		case o.Name != "Shrimp" && (!o.Included || o.PriceCents != 225):
			t.Fatalf("%s: %+v", o.Name, o)
		}
		if o.Included {
			free++
		}
	}
	if free != 4 {
		t.Fatalf("free %d", free)
	}
	// A bundle slot's free option comes after the included toppings: the
	// priciest one left (Shrimp) is the slot's.
	r = Resolve(c.Context, c.Cart.Lines[0].Selection, 1)
	if r.UnitCents != 1799 {
		t.Fatalf("with a free slot option: %d", r.UnitCents)
	}
}

func TestIncludedOf(t *testing.T) {
	n, sizes := IncludedOf(map[string]any{IncludedAttr: float64(5), IncludedSizesAttr: map[string]any{"s1": float64(2), "s2": "x", "s3": float64(-1)}})
	if n != 5 || len(sizes) != 2 || sizes["s1"] != 2 || sizes["s3"] != 0 {
		t.Fatalf("%d %v", n, sizes)
	}
	f := ItemFacts{IncludedToppings: n, IncludedSizes: sizes}
	if f.IncludedAt("s1") != 2 || f.IncludedAt("s9") != 5 || f.IncludedAt("s3") != 0 {
		t.Fatal(f)
	}
	if n, s := IncludedOf(nil); n != 0 || s != nil {
		t.Fatal(n, s)
	}
	if n, _ := IncludedOf(map[string]any{IncludedAttr: float64(500)}); n != MaxIncluded {
		t.Fatal(n)
	}
}
