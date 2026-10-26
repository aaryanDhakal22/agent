package dispatch

// AutoAssignMaxStops caps the simple auto-assign's run (2.2.1): extra orders
// wait for the next driver.
const AutoAssignMaxStops = 3

// PlanNext is the simple auto-assign (2.2.1): the plan for one driver — the
// next available one, whose pool must be empty — carrying at most
// AutoAssignMaxStops stops, with the other drivers out of the picture except
// the ones on a run (their returns feed the hold rule and the left-over
// cost). It returns his orders in driving order; nil when there is nothing
// to give him (or his pool isn't empty: staff built it, never touched).
func PlanNext(in Input, driverID string) []string {
	var drv *Driver
	for i := range in.Drivers {
		if in.Drivers[i].ID == driverID {
			drv = &in.Drivers[i]
			break
		}
	}
	if drv == nil || len(drv.Pool) > 0 {
		return nil
	}
	for _, o := range in.Orders {
		if o.PoolDriverID == driverID {
			return nil
		}
	}
	p := DefaultParams()
	if in.Params != nil {
		p = *in.Params
	}
	p.MaxStops = min(max(p.MaxStops, 1), AutoAssignMaxStops)
	in.Params = &p
	in.Drivers = []Driver{*drv}
	for _, r := range Plan(in).Runs {
		if r.DriverID == driverID && len(r.SuggestedIDs) > 0 {
			return r.SuggestedIDs
		}
	}
	return nil
}
