package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func loadInput(t *testing.T, name string) Input {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var in Input
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	return in
}

// Regression: instances from the simulator's CP-SAT cross-check.
func TestReproSmallVsBruteForce(t *testing.T) {
	in := loadInput(t, "suboptimal_seed200021.input.json")
	out := Plan(in)
	b := newBF(in)
	want := b.optimum()
	t.Logf("cands=%d free=%d locked=%d got %.3f want %.3f", len(b.cands), len(b.free), len(b.locked), out.Cost, want)
	if !near(out.Cost, want) {
		t.Fatalf("cost %.6f, optimum %.6f", out.Cost, want)
	}
}

// The CP-SAT cross-check's optima (sector rule off: that harness predates
// it) are reproduced exactly; with the rule on, the plans stay valid and
// seed200021 matches the brute force (the claimed optimum's [o03 o02]
// breaks the sector).
func TestReproCPSAT(t *testing.T) {
	cases := []struct {
		file  string
		cpsat float64 // optimum without the sector rule
	}{
		{"suboptimal_seed200021.input.json", 943.353},
		{"suboptimal_seed200002.input.json", 12333.693},
		{"suboptimal_seed200014.input.json", 18721.540},
		{"suboptimal_seed200042.input.json", 9341.177},
	}
	// The constants the cross-check ran with (the pre-tuning defaults).
	at := DefaultParams()
	at.W0, at.W1, at.W2, at.Beta, at.Lambda, at.M = 0.1, 2, 50, 1, 2, 30
	for _, c := range cases {
		in := loadInput(t, c.file)
		in.Params = &at
		out := plan(t, in)
		p := at
		p.SectorDeg = 360
		in.Params = &p
		off := plan(t, in)
		if diff := off.Cost - c.cpsat; diff > 1e-3 || diff < -1e-3 {
			t.Fatalf("%s: sector off %.3f, CP-SAT %.3f", c.file, off.Cost, c.cpsat)
		}
		if out.Cost < off.Cost-1e-9 {
			t.Fatalf("%s: a tighter rule can't be cheaper", c.file)
		}
	}
	b := newBF(loadInput(t, "suboptimal_seed200021.input.json"))
	if !b.sectorOK([]string{"o05", "o01", "o07"}) || b.sectorOK([]string{"o03", "o02"}) {
		t.Fatal("sector check of the claimed optimum")
	}
}

func TestReproOver25(t *testing.T) {
	in := loadInput(t, "over_25_waiting.input.json")
	out := plan(t, in)
	if out.Cost > 10981.957+1e-6 {
		t.Fatalf("cost %.3f, CP-SAT proved 10981.957 under the old rules", out.Cost)
	}
	t.Logf("cost %.3f in %d ms", out.Cost, out.SolveMs)
}
