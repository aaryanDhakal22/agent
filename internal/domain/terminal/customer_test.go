package terminal

import "testing"

func TestCustomerHelpers(t *testing.T) {
	for in, want := range map[string]string{"(410) 555-0101": "4105550101", "+1 410 555 0101": "4105550101", "555-0101": "",
		"0105550101": "", "14105550101": "4105550101"} {
		if got := NormPhone(in); got != want {
			t.Errorf("NormPhone(%q) = %q", in, got)
		}
	}
	if Reverse("4105550101") != "1010555014" {
		t.Error("Reverse")
	}
	if !SameAddress("12 North Main Street", "21286-1234", "12 n. main st", "") || SameAddress("12 Main St", "21286", "12 Main St", "21204") {
		t.Error("SameAddress")
	}
	const r = 4828 // 3 mi
	for _, c := range []struct {
		mi   float64
		want string
	}{{2.9, AreaIn}, {3.5, AreaNear}, {4.2, AreaOut}} {
		a := AreaOf(c.mi*metersPerMile, r, 1)
		if a.State != c.want || *a.DistanceMi != c.mi {
			t.Errorf("%v mi: %+v", c.mi, a)
		}
	}
	if a := AreaOf(3.5*metersPerMile, r, 0); a.State != AreaOut {
		t.Errorf("no margin: %+v", a)
	}
	if d := DistanceM(39.4, -76.6, 39.4+1/69.0, -76.6); d < 1600 || d > 1620 {
		t.Errorf("a mile north: %v m", d)
	}
}
