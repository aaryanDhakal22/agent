package dispatch

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
)

// denseInstance: n ready orders all in one 60-degree sector (every bundle
// fits the sector and most the detour limit) and k free drivers — the
// search's worst case.
func denseInstance(seed uint64, n, k int) Input {
	rng := rand.New(rand.NewPCG(seed, 7))
	w := newWorld()
	w.rng, w.noise = rng, 0.05
	for i := range n {
		ang := (rng.Float64() - 0.5) * math.Pi / 3
		rad := 500 + 3500*rng.Float64()
		w.order(fmt.Sprintf("o%02d", i), rad*math.Cos(ang), rad*math.Sin(ang), rng.Float64()*30, -1)
	}
	for d := range k {
		w.driver(fmt.Sprintf("d%d", d))
	}
	in := w.fill()
	in.BudgetMs = 100000
	return in
}

// BenchmarkPlanDense25x6: the brief's worst case, one instance.
func BenchmarkPlanDense25x6(b *testing.B) {
	in := denseInstance(1, 25, 6)
	b.ReportAllocs()
	for b.Loop() {
		if Plan(in).Fallback {
			b.Fatal("fallback")
		}
	}
}

// BenchmarkPlanDense25 runs 40 dense instances per op for each driver
// count (ns/op / 40 = mean solve time).
func BenchmarkPlanDense25(b *testing.B) {
	for _, k := range []int{2, 4, 6, 8} {
		ins := make([]Input, 40)
		for s := range ins {
			ins[s] = denseInstance(uint64(s+100), 25, k)
		}
		b.Run(fmt.Sprintf("k=%d", k), func(b *testing.B) {
			for b.Loop() {
				for _, in := range ins {
					if Plan(in).Fallback {
						b.Fatal("fallback")
					}
				}
			}
		})
	}
}

// BenchmarkPlanRepro: the simulator's 25-waiting backlog (real OSRM times).
func BenchmarkPlanRepro(b *testing.B) {
	in := loadInput(&testing.T{}, "over_25_waiting.input.json")
	for b.Loop() {
		Plan(in)
	}
}
