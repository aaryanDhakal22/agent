package dispatch

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

// TestOptimalVsBruteForce: on random small instances the optimizer's cost
// equals the brute-force optimum over every assignment of orders to
// drivers (or left over), and the plan it emits really costs that.
func TestOptimalVsBruteForce(t *testing.T) {
	n := 2500
	if testing.Short() {
		n = 300
	}
	rng := rand.New(rand.NewPCG(20261022, 1))
	worst, pooled := 0, 0
	byDrivers := map[int]int{}
	for it := range n {
		size, drivers := 1+rng.IntN(8), 1+rng.IntN(3)
		switch it % 10 {
		case 0: // bigger, fewer drivers
			size, drivers = 9+rng.IntN(2), 1+rng.IntN(2)
		case 5: // many drivers: the balance term and turn order bite
			size, drivers = 6+rng.IntN(3), 4
		}
		radius := 800 + rng.Float64()*5000
		in := randomInstance(rng, size, drivers, radius)
		if it%3 == 1 { // other tunings: the rules must hold whatever the weights
			p := DefaultParams()
			p.Lambda = []float64{0, 2, 8, 40}[rng.IntN(4)]
			p.M = []float64{0, 5, 30}[rng.IntN(3)]
			p.W0 = []float64{0, 0.1, 1}[rng.IntN(3)]
			p.Beta = []float64{0.2, 1, 3}[rng.IntN(3)]
			p.SectorDeg = []float64{45, 90, 360}[rng.IntN(3)]
			p.SectorMinDistM = []float64{0, 500, 1500}[rng.IntN(3)]
			in.Params = &p
		}
		out := Plan(in)
		if out.Fallback {
			t.Fatalf("instance %d: fallback", it)
		}
		b := newBF(in)
		want := b.optimum()
		if len(b.locked)+len(b.free) == 0 || len(b.cands)+len(b.locked) == 0 {
			continue
		}
		if !near(out.Cost, want) {
			js, _ := json.Marshal(in)
			t.Fatalf("instance %d: cost %.6f, brute force %.6f\n%s", it, out.Cost, want, js)
		}
		if got := b.planCost(out); !near(got, out.Cost) {
			js, _ := json.Marshal(in)
			t.Fatalf("instance %d: emitted plan costs %.6f, reported %.6f\n%s", it, got, out.Cost, js)
		}
		worst = max(worst, len(b.cands))
		byDrivers[len(b.free)+len(b.locked)]++
		if len(b.locked) > 0 {
			pooled++
		}
	}
	t.Logf("%d instances, up to %d candidates, by driver count %v, %d with pools: optimizer == brute force", n, worst, byDrivers, pooled)
}
