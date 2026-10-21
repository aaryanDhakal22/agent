package printer

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Test print + probe for the tablet's setup wizard and printer guide. Unlike
// Execute/PrintDocument there is NO failover: a slip meant for printer X must
// never come out of printer Y, or staff would conclude X works.

// Test-result error codes (stable: the tablet branches on them).
const (
	TestUnreachable   = "unreachable"    // TCP dial failed (off, unplugged, wrong IP)
	TestWriteFailed   = "write_failed"   // connected, but the job didn't go through
	TestNotConfigured = "not_configured" // unknown printer id, or no address set
)

// testBudget bounds one printer's test (dial + write); printers run in
// parallel, so a dead one doesn't delay the others.
const testBudget = 12 * time.Second

// TestResult is one printer's answer to a test print or probe.
type TestResult struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	ErrorCode string `json:"error_code,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Roster returns the configured printers in roster order.
func (d *Dispatcher) Roster() []RosterEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]RosterEntry, len(d.roster))
	for i, sl := range d.roster {
		out[i] = sl.entry
	}
	return out
}

// PrintTest prints build(entry) on every printer (printerID == "") or on the
// one with that id, each only on itself. Results are in roster order.
func (d *Dispatcher) PrintTest(ctx context.Context, printerID string, build func(RosterEntry) []byte) []TestResult {
	return d.eachPrinter(ctx, printerID, func(ctx context.Context, sl rosterSlot) error {
		return sl.svc.PrintBytes(ctx, "test-slip", build(sl.entry))
	})
}

// Probe re-checks reachability now (the registry is updated too) without
// printing anything.
func (d *Dispatcher) Probe(ctx context.Context, printerID string) []TestResult {
	return d.eachPrinter(ctx, printerID, func(ctx context.Context, sl rosterSlot) error {
		if err := sl.svc.ProbeNow(ctx); err != nil {
			return errors.Join(ErrPrinterUnreachable, err)
		}
		return nil
	})
}

func (d *Dispatcher) eachPrinter(ctx context.Context, printerID string, run func(context.Context, rosterSlot) error) []TestResult {
	d.mu.RLock()
	roster := append([]rosterSlot(nil), d.roster...)
	d.mu.RUnlock()
	var slots []rosterSlot
	for _, sl := range roster {
		if printerID == "" || sl.entry.ID == printerID {
			slots = append(slots, sl)
		}
	}
	if printerID != "" && len(slots) == 0 {
		return []TestResult{{ID: printerID, ErrorCode: TestNotConfigured, Error: "no printer with that id"}}
	}
	out := make([]TestResult, len(slots))
	var wg sync.WaitGroup
	for i, sl := range slots {
		out[i] = TestResult{ID: sl.entry.ID, Name: sl.entry.Name}
		if sl.svc.Printer().IP() == "" {
			out[i].ErrorCode, out[i].Error = TestNotConfigured, "no address set for this printer"
			continue
		}
		wg.Add(1)
		go func(i int, sl rosterSlot) {
			defer wg.Done()
			tctx, cancel := context.WithTimeout(ctx, testBudget)
			defer cancel()
			err := run(tctx, sl)
			switch {
			case err == nil:
				out[i].OK = true
			case errors.Is(err, ErrPrinterUnreachable):
				out[i].ErrorCode, out[i].Error = TestUnreachable, err.Error()
			default:
				out[i].ErrorCode, out[i].Error = TestWriteFailed, err.Error()
			}
		}(i, sl)
	}
	wg.Wait()
	return out
}
