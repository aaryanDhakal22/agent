package pricing

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

// Lombardi's real coupons and prices (testdata/lombardis_coupons.json: prod's
// counter pricing context of 2026-11-13, cut to the coupons' families): a
// cart per coupon as the customer names it ("12\" Pizza + 8 Party Wings" on a
// plain 12" + 8 wings, the 2026-11-13 prod report), with hand-worked
// expectations: the subtotal, the coupons applied (by name, in order), the
// offers (by name, in order) and the warnings. -update records each full
// quote; the TS engine (frontend tests/pricing.test.mjs) and the flows model
// (tests/flows/unit_model.py) must give the same quotes.
type lombardisDoc struct {
	At      time.Time       `json:"at"`
	Context Context         `json:"context"`
	Cases   []lombardisCase `json:"cases"`
}

type lombardisCase struct {
	Name   string          `json:"name"`
	Cart   Cart            `json:"cart"`
	Expect lombardisExpect `json:"expect"`
	Quote  json.RawMessage `json:"quote,omitempty"`
}

type lombardisExpect struct {
	SubtotalCents int64     `json:"subtotal_cents"`
	Coupons       *[]string `json:"coupons"`
	Offers        *[]string `json:"offers"`
	Warnings      *[]string `json:"warnings"`
}

const lombardisFile = "testdata/lombardis_coupons.json"

func TestLombardisCoupons(t *testing.T) {
	raw, err := os.ReadFile(lombardisFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc lombardisDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	coupons := 0
	for _, d := range doc.Context.Deals {
		if d.Kind == DealCoupon {
			coupons++
		}
	}
	if coupons < 18 {
		t.Fatalf("only %d coupons in the fixture", coupons)
	}
	quotes := make([]map[string]any, len(doc.Cases))
	for i, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			q := Price(doc.Context, c.Cart, doc.At)
			if len(q.Errors) != 0 {
				t.Errorf("errors: %+v", q.Errors)
			}
			if q.SubtotalCents != c.Expect.SubtotalCents {
				t.Errorf("subtotal %d, want %d", q.SubtotalCents, c.Expect.SubtotalCents)
			}
			if c.Expect.Coupons != nil {
				got := []string{}
				for _, a := range q.Coupons {
					got = append(got, a.Name)
				}
				if !reflect.DeepEqual(got, *c.Expect.Coupons) {
					t.Errorf("coupons applied %q, want %q (warnings %q)", got, *c.Expect.Coupons, q.Warnings)
				}
			}
			if c.Expect.Offers != nil {
				got := []string{}
				for _, o := range q.CouponOffers {
					got = append(got, o.Name)
				}
				if !reflect.DeepEqual(got, *c.Expect.Offers) {
					t.Errorf("offers %q, want %q", got, *c.Expect.Offers)
				}
			}
			if c.Expect.Warnings != nil && !reflect.DeepEqual(append([]string{}, q.Warnings...), *c.Expect.Warnings) {
				t.Errorf("warnings %q, want %q", q.Warnings, *c.Expect.Warnings)
			}
			// Every applied coupon's lines add up: the subtotal is the lines as sold.
			var sum int64
			for _, l := range q.Lines {
				sum += l.TotalCents
			}
			if sum != q.SubtotalCents {
				t.Errorf("lines add up to %d, subtotal %d", sum, q.SubtotalCents)
			}
			got, _ := json.Marshal(q)
			var gotMap map[string]any
			_ = json.Unmarshal(got, &gotMap)
			quotes[i] = gotMap
			if *update {
				return
			}
			if len(c.Quote) == 0 {
				t.Fatalf("no recorded quote (run with -update)")
			}
			var want map[string]any
			_ = json.Unmarshal(c.Quote, &want)
			if !reflect.DeepEqual(want, gotMap) {
				t.Errorf("quote changed:\n got %s\nwant %s", got, c.Quote)
			}
		})
	}
	if *update {
		var full map[string]any
		_ = json.Unmarshal(raw, &full)
		cases := full["cases"].([]any)
		for i := range cases {
			cases[i].(map[string]any)["quote"] = quotes[i]
		}
		out, _ := json.MarshalIndent(full, "", " ")
		if err := os.WriteFile(lombardisFile, append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
