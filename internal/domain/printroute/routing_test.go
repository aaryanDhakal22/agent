package printroute

import (
	"errors"
	"reflect"
	"testing"
)

var known = map[string]bool{"front": true, "pizza": true, "wings": true, "spare": true}

func TestDecodeEmptyIsNone(t *testing.T) {
	r, err := Decode(nil)
	if err != nil || r.Mode != ModeNone || r.RuleTargets("pizza") != nil {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}

func TestNormalizeDropsSelfDupesAndBlanks(t *testing.T) {
	r := Routing{Mode: " per_printer ", Targets: map[string][]string{
		"wings": {"spare", " spare", "wings", "", "pizza"},
		"pizza": {"pizza"},
		" ":     {"front"},
	}}.Normalize()
	if r.Mode != ModePerPrinter || !reflect.DeepEqual(r.Targets, map[string][]string{"wings": {"spare", "pizza"}}) {
		t.Fatalf("got %+v", r)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		r    Routing
		code string
	}{
		{Routing{Mode: "sometimes"}, "bad_mode"},
		{Routing{Mode: ModeOne}, "no_emergency_printer"},
		{Routing{Mode: ModeOne, EmergencyPrinterID: "gone"}, "unknown_printer"},
		{Routing{Mode: ModePerPrinter, Targets: map[string][]string{"gone": {"spare"}}}, "unknown_printer"},
		{Routing{Mode: ModePerPrinter, Targets: map[string][]string{"wings": {"gone"}}}, "unknown_printer"},
		{Routing{Mode: ModeNone}, ""},
		{Routing{Mode: ModeOne, EmergencyPrinterID: "spare"}, ""},
		{Routing{Mode: ModePerPrinter, Targets: map[string][]string{"wings": {"spare", "pizza"}}}, ""},
		// A stale per-printer list doesn't block mode none (it is kept, not used)
		// only while its ids still exist.
		{Routing{Mode: ModeNone, EmergencyPrinterID: "spare"}, ""},
	}
	for i, c := range cases {
		err := c.r.Normalize().Validate(known)
		var ve *ValidationError
		switch {
		case c.code == "" && err != nil:
			t.Errorf("%d: unexpected %v", i, err)
		case c.code != "" && (!errors.As(err, &ve) || ve.Code != c.code || !errors.Is(err, ErrInvalid)):
			t.Errorf("%d: want %s, got %v", i, c.code, err)
		}
	}
}

func TestRuleTargets(t *testing.T) {
	one := Routing{Mode: ModeOne, EmergencyPrinterID: "spare", Targets: map[string][]string{"wings": {"pizza"}}}
	if got := one.RuleTargets("wings"); !reflect.DeepEqual(got, []string{"spare"}) {
		t.Fatalf("one: %v", got)
	}
	if got := one.RuleTargets("spare"); got != nil {
		t.Fatalf("the emergency printer itself has no rule: %v", got)
	}
	per := one
	per.Mode = ModePerPrinter
	if got := per.RuleTargets("wings"); !reflect.DeepEqual(got, []string{"pizza"}) {
		t.Fatalf("per: %v", got)
	}
	if got := per.RuleTargets("front"); got != nil {
		t.Fatalf("no list = no rule: %v", got)
	}
	none := one
	none.Mode = ModeNone
	if got := none.RuleTargets("wings"); got != nil {
		t.Fatalf("none: %v", got)
	}
}
