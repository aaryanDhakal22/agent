package dispatch

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// world is a synthetic planar town for tests: positions are meters east (x)
// and north (y) of the store, drive time is Manhattan distance / speed
// (road-ish, metric), optionally with asymmetric noise.
type world struct {
	in    Input
	pos   map[string][2]float64
	speed float64
	noise float64
	rng   *rand.Rand
}

var (
	t0      = time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)
	storeLL = LatLng{Lat: 40.6687, Lng: -74.1143}
)

func newWorld() *world {
	return &world{
		in: Input{
			Now: t0, TargetMinutes: 45, SoonReadyMinutes: 5, Store: &LatLng{storeLL.Lat, storeLL.Lng},
			Orders: []Order{}, Drivers: []Driver{}, BudgetMs: 2000,
		},
		pos:   map[string][2]float64{"": {0, 0}},
		speed: 8,
	}
}

func toLL(x, y float64) LatLng {
	lat := storeLL.Lat + y/111320
	lng := storeLL.Lng + x/(111320*math.Cos(storeLL.Lat*math.Pi/180))
	return LatLng{lat, lng}
}

// order adds a located order at (x, y), placed agoMin minutes before now,
// ready now (readyIn <= 0) or in readyIn minutes.
func (w *world) order(id string, x, y, agoMin, readyIn float64) *Order {
	ll := toLL(x, y)
	o := Order{ID: id, PlacedAt: t0.Add(-time.Duration(agoMin * float64(time.Minute))), Lat: ll.Lat, Lng: ll.Lng, Located: true}
	if readyIn <= 0 {
		o.Ready = true
	} else {
		o.ReadyAt = t0.Add(time.Duration(readyIn * float64(time.Minute)))
	}
	w.in.Orders = append(w.in.Orders, o)
	w.pos[id] = [2]float64{x, y}
	return &w.in.Orders[len(w.in.Orders)-1]
}

func (w *world) driver(id string, pool ...string) {
	w.in.Drivers = append(w.in.Drivers, Driver{ID: id, Name: "D " + id, Pool: pool})
	for _, p := range pool {
		for i := range w.in.Orders {
			if w.in.Orders[i].ID == p {
				w.in.Orders[i].PoolDriverID = id
			}
		}
	}
}

func (w *world) drive(a, b string) int {
	pa, pb := w.pos[a], w.pos[b]
	d := math.Abs(pa[0]-pb[0]) + math.Abs(pa[1]-pb[1])
	s := d / w.speed
	if w.noise > 0 && a != b {
		s *= 1 + w.noise*w.rng.Float64()
	}
	return int(math.Round(s))
}

// fill writes the full matrix for every located order.
func (w *world) fill() Input {
	t := Times{Store: StoreTimes{To: map[string]int{}, From: map[string]int{}}, Pairs: map[string]map[string]int{}}
	for _, a := range w.in.Orders {
		if !a.Located {
			continue
		}
		t.Store.To[a.ID] = w.drive("", a.ID)
		t.Store.From[a.ID] = w.drive(a.ID, "")
		t.Pairs[a.ID] = map[string]int{}
		for _, b := range w.in.Orders {
			if b.Located && b.ID != a.ID {
				t.Pairs[a.ID][b.ID] = w.drive(a.ID, b.ID)
			}
		}
	}
	w.in.Times = t
	return w.in
}

// randomInstance: n orders within radius r meters, k drivers (some with a
// pool), a few unlocated / cooking orders, maybe out drivers.
func randomInstance(rng *rand.Rand, n, k int, r float64) Input {
	w := newWorld()
	w.rng, w.noise = rng, 0.1*rng.Float64()
	for i := range n {
		ang, rad := rng.Float64()*2*math.Pi, r*math.Sqrt(rng.Float64())
		ready := -1.0
		if rng.Float64() < 0.3 {
			ready = rng.Float64() * 9
		}
		o := w.order(fmt.Sprintf("o%02d", i), rad*math.Cos(ang), rad*math.Sin(ang), rng.Float64()*60, ready)
		if rng.Float64() < 0.08 {
			o.Located = false
		}
		if rng.Float64() < 0.1 { // duplicate address
			j := rng.IntN(i + 1)
			o.Lat, o.Lng = w.in.Orders[j].Lat, w.in.Orders[j].Lng
			w.pos[o.ID] = w.pos[w.in.Orders[j].ID]
		}
	}
	used := map[int]bool{}
	for d := range k {
		var pool []string
		if rng.Float64() < 0.3 {
			for range 1 + rng.IntN(2) {
				oi := rng.IntN(max(n, 1))
				if n > 0 && !used[oi] && w.in.Orders[oi].Located {
					used[oi] = true
					pool = append(pool, w.in.Orders[oi].ID)
				}
			}
		}
		w.driver(fmt.Sprintf("d%d", d), pool...)
		// Pools come both ways on the wire: the order's pool_driver_id
		// and the driver's list; exercise each alone.
		dr := &w.in.Drivers[len(w.in.Drivers)-1]
		var keep []string
		for _, id := range dr.Pool {
			switch rng.IntN(3) {
			case 0: // only on the driver
				for i := range w.in.Orders {
					if w.in.Orders[i].ID == id {
						w.in.Orders[i].PoolDriverID = ""
					}
				}
				keep = append(keep, id)
			case 1: // only on the order
			default:
				keep = append(keep, id)
			}
		}
		dr.Pool = keep
	}
	if rng.Float64() < 0.5 {
		for i := range 1 + rng.IntN(2) {
			w.in.OutDrivers = append(w.in.OutDrivers, OutDriver{ID: fmt.Sprintf("x%d", i), ReturnAt: t0.Add(time.Duration(rng.IntN(30*60)) * time.Second)})
		}
	}
	return w.fill()
}
