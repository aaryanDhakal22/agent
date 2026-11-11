package pricing

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// -update records each case's full quote (the TypeScript engine must match
// it exactly); the hand-worked `expect` values are never written by a test.
var update = flag.Bool("update", false, "record the full quote of every case")

type pricingCase struct {
	Name    string          `json:"name"`
	At      time.Time       `json:"at"`
	Context Context         `json:"context"`
	Cart    Cart            `json:"cart"`
	Expect  map[string]any  `json:"expect"`
	Quote   json.RawMessage `json:"quote,omitempty"`
}

func TestSharedCases(t *testing.T) {
	files, _ := filepath.Glob("testdata/cases/*.json")
	if len(files) < 20 {
		t.Fatalf("only %d cases", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c pricingCase
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		t.Run(c.Name, func(t *testing.T) {
			q := Price(c.Context, c.Cart, c.At)
			got, _ := json.Marshal(q)
			var gotMap map[string]any
			_ = json.Unmarshal(got, &gotMap)
			checkExpect(t, c.Expect, gotMap)
			if *update {
				var full map[string]any
				_ = json.Unmarshal(raw, &full)
				full["quote"] = gotMap
				out, _ := json.MarshalIndent(full, "", " ")
				if err := os.WriteFile(f, append(out, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
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
}

// checkExpect compares the hand-worked fields: plain values exactly, lines /
// discounts as lists of partial objects, error_codes / error_messages
// against the errors in order.
func checkExpect(t *testing.T, exp, got map[string]any) {
	t.Helper()
	errs, _ := got["errors"].([]any)
	for k, want := range exp {
		switch k {
		case "error_codes", "error_messages":
			field := map[string]string{"error_codes": "code", "error_messages": "message"}[k]
			var have []any
			for _, e := range errs {
				have = append(have, e.(map[string]any)[field])
			}
			if !reflect.DeepEqual(want, have) && !(len(want.([]any)) == 0 && len(have) == 0) {
				t.Errorf("%s: got %v want %v", k, have, want)
			}
		case "lines", "discounts":
			w, h := want.([]any), got[k].([]any)
			if len(w) != len(h) {
				t.Errorf("%s: got %d want %d: %v", k, len(h), len(w), h)
				continue
			}
			for i := range w {
				for f, v := range w[i].(map[string]any) {
					if hv := h[i].(map[string]any)[f]; !reflect.DeepEqual(hv, v) {
						t.Errorf("%s[%d].%s: got %v want %v", k, i, f, hv, v)
					}
				}
			}
		default:
			if g := got[k]; !reflect.DeepEqual(g, want) {
				t.Errorf("%s: got %v want %v", k, g, want)
			}
		}
	}
}
