package dispatch

import "math"

// lpCol is one LP column: a 0/1 column with up to 6 ones.
type lpCol struct {
	rows [6]int16
	n    uint8
	cost float64
}

// solveLP minimises cost·x subject to A x = b, x >= 0, where the first m
// columns are the identity (a feasible starting basis since b >= 0). It is
// a dense revised simplex with an explicit basis inverse — the LPs here
// have ~30 rows and a few thousand 0/1 columns. Partial Dantzig pricing,
// switching to Bland's rule after a run of degenerate pivots so it cannot
// cycle.
// Returns the optimum, the row duals and the primal values; ok is false if the iteration cap
// was hit (the caller then uses weaker multipliers).
func solveLP(m int, b []float64, cols []lpCol) (val float64, pi, x []float64, ok bool) {
	const eps = 1e-9
	binv := make([]float64, m*m)
	for i := range m {
		binv[i*m+i] = 1
	}
	basis := make([]int, m)
	inBasis := make([]bool, len(cols))
	for i := range m {
		basis[i], inBasis[i] = i, true
	}
	// Perturb the right-hand side (deterministically) against the heavy
	// degeneracy of set partitioning; the duals stay dual feasible, and the
	// bound reported is b·pi with the true b.
	xB := make([]float64, m)
	for i := range m {
		xB[i] = b[i] + 1e-7*(1+float64((i*7919)%101)/101)
	}
	pi = make([]float64, m)
	d := make([]float64, m)
	bland, degen := false, 0
	chunk, cursor := max(64, len(cols)/8), 0
	for it := 0; it < 20*m+2000; it++ {
		for r := range m {
			s := 0.0
			for i := range m {
				s += cols[basis[i]].cost * binv[i*m+r]
			}
			pi[r] = s
		}
		// Partial pricing: the most negative reduced cost within a chunk of
		// columns, moving on chunk by chunk (a full cycle with none means
		// optimal). Bland's rule: the first improving column.
		enter, bestRC := -1, -eps
		if bland {
			for q := range cols {
				if !inBasis[q] && rcOf(&cols[q], pi) < -eps {
					enter = q
					break
				}
			}
		} else {
			for scanned := 0; scanned < len(cols) && enter < 0; {
				end := min(cursor+chunk, len(cols))
				for q := cursor; q < end; q++ {
					if inBasis[q] {
						continue
					}
					if rc := rcOf(&cols[q], pi); rc < bestRC {
						enter, bestRC = q, rc
					}
				}
				scanned += end - cursor
				cursor = end
				if cursor == len(cols) {
					cursor = 0
				}
			}
		}
		if enter < 0 {
			val = 0
			for i := range m {
				val += pi[i] * b[i]
			}
			x = make([]float64, len(cols))
			for i := range m {
				x[basis[i]] = xB[i]
			}
			return val, pi, x, true
		}
		c := &cols[enter]
		for i := range m {
			s := 0.0
			for k := range c.n {
				s += binv[i*m+int(c.rows[k])]
			}
			d[i] = s
		}
		p, ratio := -1, math.Inf(1)
		for i := range m {
			if d[i] > eps {
				r := xB[i] / d[i]
				if r < ratio-eps || (r < ratio+eps && p >= 0 && basis[i] < basis[p]) {
					p, ratio = i, r
				}
			}
		}
		if p < 0 {
			return 0, nil, nil, false // unbounded: impossible for these LPs
		}
		if ratio < eps {
			if degen++; degen > 25 {
				bland = true
			}
		} else {
			degen, bland = 0, false
		}
		// Pivot on (p, enter).
		dp := d[p]
		rowP := binv[p*m : p*m+m]
		for r := range m {
			rowP[r] /= dp
		}
		xB[p] /= dp
		for i := range m {
			if i == p || d[i] == 0 {
				continue
			}
			f := d[i]
			row := binv[i*m : i*m+m]
			for r := range m {
				row[r] -= f * rowP[r]
			}
			xB[i] -= f * xB[p]
			if xB[i] < 0 && xB[i] > -1e-12 {
				xB[i] = 0
			}
		}
		inBasis[basis[p]] = false
		basis[p], inBasis[enter] = enter, true
	}
	return 0, nil, nil, false
}

func rcOf(c *lpCol, pi []float64) float64 {
	rc := c.cost
	for k := range c.n {
		rc -= pi[c.rows[k]]
	}
	return rc
}
