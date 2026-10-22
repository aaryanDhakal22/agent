package dispatch

import "encoding/json"

// Model constants (the plan's fixed rules).
const (
	// MaxStops is the most deliveries one run may carry.
	MaxStops = 4
	// DetourMinS / DetourFactor: each stop's in-car time (departure -> its
	// door, incl. earlier stops' service) may be at most
	// max(direct + DetourMinS, DetourFactor * direct).
	DetourMinS   = 480
	DetourFactor = 1.5
	// ServiceS is the time spent at each door (parking, hand-over).
	ServiceS = 120
	// MaxCandidates caps the exact search; older orders win the slots.
	MaxCandidates = 25
	// SectorDeg / SectorMinDistM: "same direction". Seen from the store,
	// the stops of a run farther than SectorMinDistM must all lie within one
	// SectorDeg-wide sector (a quadrant); closer stops are exempt. Applies on
	// top of the detour limit; needs the store pin (skipped without one).
	SectorDeg      = 90
	SectorMinDistM = 500
)

// Params are the tuning constants. Costs are in "minutes of badness": one
// unit is what one minute of a driver's time is worth (Beta = 1).
//
//	late(x) per stop, x = placed-to-door minutes, T = target:
//	  W0*x                                  always (earlier is better: tie-breaker)
//	  + 0                                   x <= T-UrgentMin  (safe: optimize driving)
//	  + W1*(x-(T-UrgentMin))                T-UrgentMin < x <= T (urgency rises)
//	  + W1*UrgentMin + W2*(x-T)             x > T (late: top priority, even alone)
//	run   = sum late(stops) + Beta * run minutes (out + service + back)
//	plan  = sum runs + Lambda * (most stops - fewest stops in a run, when >= 2 runs)
//	        + sum over left-over orders [late(door time if it waits for the
//	          next return) + Beta * its solo run minutes + M]
//
// The balance term counts stops (deliveries per driver: what makes 3+3 beat
// 4+2), not minutes: a minutes spread is invisible to every additive bound,
// so an exact search over it explodes on dense nights; a stop spread has at
// most a handful of values and is searched exactly window by window.
//
// The left-over term charges the solo trip it will need later anyway, so
// leaving an order behind never "saves" driving; M is the price of not
// sending it now.
type Params struct {
	MaxStops      int     `json:"max_stops"`
	DetourMinS    float64 `json:"detour_min_s"`
	DetourFactor  float64 `json:"detour_factor"`
	ServiceS      float64 `json:"service_s"`
	MaxCandidates int     `json:"max_candidates"`
	// SectorDeg >= 360 turns the sector rule off.
	SectorDeg      float64 `json:"sector_deg"`
	SectorMinDistM float64 `json:"sector_min_dist_m"`

	W0        float64 `json:"w0"`
	W1        float64 `json:"w1"`
	W2        float64 `json:"w2"`
	UrgentMin float64 `json:"urgent_min"`
	Beta      float64 `json:"beta"`
	Lambda    float64 `json:"lambda"`
	M         float64 `json:"m"`

	// TypicalRunMin: how long a left-over order is assumed to wait when no
	// driver is out (no return time to go by).
	TypicalRunMin float64 `json:"typical_run_min"`
	// Fallback drive-time estimate for a missing pair: straight-line meters
	// * RoadFactor / SpeedMps, unless a factor can be learned from the
	// known pairs of this request.
	RoadFactor float64 `json:"road_factor"`
	SpeedMps   float64 `json:"speed_mps"`
}

// DefaultParams are the shipped constants, tuned once by the simulator
// (quicc-ops sim/dispatch, Towson world) with this package's rule tests as a
// hard gate (DISPATCH_TEST_PARAMS). The steep urgency ramp (W1 over the last
// UrgentMin before the target) is what makes a late order go alone: by the
// target it already carries W1*UrgentMin, so W2 needn't dwarf it.
func DefaultParams() Params {
	return Params{
		MaxStops:       MaxStops,
		DetourMinS:     DetourMinS,
		DetourFactor:   DetourFactor,
		ServiceS:       ServiceS,
		MaxCandidates:  MaxCandidates,
		SectorDeg:      SectorDeg,
		SectorMinDistM: SectorMinDistM,
		W0:             0.1,
		W1:             12,
		W2:             10,
		UrgentMin:      10,
		Beta:           1,
		Lambda:         2,
		M:              30,
		TypicalRunMin:  25,
		RoadFactor:     1.35,
		SpeedMps:       8.5,
	}
}

// UnmarshalJSON decodes over DefaultParams, so omitted fields keep their
// default and an explicit 0 is honoured.
func (p *Params) UnmarshalJSON(b []byte) error {
	type plain Params
	v := plain(DefaultParams())
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*p = Params(v)
	return nil
}

// sane clamps values the solver can't work with back to the defaults.
func (p Params) sane() Params {
	d := DefaultParams()
	if p.MaxStops < 1 || p.MaxStops > MaxStops {
		p.MaxStops = d.MaxStops
	}
	if p.MaxCandidates < 1 || p.MaxCandidates > 32 { // candidate sets are uint32 masks
		p.MaxCandidates = d.MaxCandidates
	}
	if p.DetourMinS < 0 {
		p.DetourMinS = d.DetourMinS
	}
	if p.DetourFactor < 1 {
		p.DetourFactor = d.DetourFactor
	}
	if p.ServiceS < 0 {
		p.ServiceS = d.ServiceS
	}
	for _, f := range []*float64{&p.W0, &p.W1, &p.W2, &p.UrgentMin, &p.Beta, &p.Lambda, &p.M} {
		if *f < 0 {
			*f = 0
		}
	}
	if p.TypicalRunMin <= 0 {
		p.TypicalRunMin = d.TypicalRunMin
	}
	if p.RoadFactor <= 0 {
		p.RoadFactor = d.RoadFactor
	}
	if p.SectorDeg <= 0 {
		p.SectorDeg = d.SectorDeg
	}
	if p.SectorMinDistM < 0 {
		p.SectorMinDistM = d.SectorMinDistM
	}
	if p.SpeedMps <= 0 {
		p.SpeedMps = d.SpeedMps
	}
	return p
}

// late is the per-stop cost of a placed-to-door time of x minutes.
func (p *Params) late(x, target float64) float64 {
	c := p.W0 * x
	if s := target - p.UrgentMin; x > s {
		if x <= target {
			c += p.W1 * (x - s)
		} else {
			c += p.W1*p.UrgentMin + p.W2*(x-target)
		}
	}
	return c
}
