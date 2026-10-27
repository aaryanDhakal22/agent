package order

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// 2.2.3: driver reports any time, merged with the hub's POS driver ledger.

type docLog struct {
	mu    sync.Mutex
	slips []order.DriverReport
	fail  bool
}

func (d *docLog) wire(svc *Service) {
	svc.SetDocumentPrinting(func(_ context.Context, _ string, build func(string) []byte) (printer.TicketResult, error) {
		_ = build("")
		if d.fail {
			return printer.TicketResult{Err: "printer offline"}, context.DeadlineExceeded
		}
		return printer.TicketResult{PrintedOn: "Front"}, nil
	}, func(rep order.DriverReport, _ string) []byte {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.slips = append(d.slips, rep)
		return []byte("slip")
	})
}

func (d *docLog) last(t *testing.T) order.DriverReport {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.slips) == 0 {
		t.Fatal("nothing printed")
	}
	return d.slips[len(d.slips)-1]
}

// deliveredPOS puts a POS delivery out with the driver whose number the
// terminal set, then brings the driver back (delivered).
func deliveredPOS(t *testing.T, svc *Service, ref, pos, driverID string) {
	t.Helper()
	e := extOrder("delivery", "h-"+ref)
	e.DriverPosNumber = &pos
	mustUpsert(t, svc, ref, e)
	if _, err := svc.DriverReturned(context.Background(), driverID); err != nil {
		t.Fatal(err)
	}
}

func hubConnected(t *testing.T, svc *Service) {
	t.Helper()
	if err := svc.SetIntegrationStatus(context.Background(), order.IntegrationStatus{OK: true, InsertMode: "off"}); err != nil {
		t.Fatal(err)
	}
}

func lcheck(ref string, n int64, pos string) order.LedgerCheck {
	out := time.Now().Add(-10 * time.Minute).UTC()
	return order.LedgerCheck{Check: ref, Number: n, DriverPosNumber: pos, Closed: true, CheckAt: time.Now().UTC(),
		OutAt: &out, Paid: true, TipCents: 300, TotalCents: 2400, CardCents: 2700}
}

func TestDriverReportMergesTheLedger(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	juma, _ := svc.CreateDriver(ctx, "Juma", "31")
	if _, err := svc.ClockIn(ctx, juma.ID); err != nil {
		t.Fatal(err)
	}
	deliveredPOS(t, svc, "1001", "31", juma.ID)

	// No hub has ever reported: the report is as before (no POS block).
	rep, err := svc.DriverReport(ctx, juma.ID)
	if err != nil || rep.POS != nil || len(rep.Lines) != 1 {
		t.Fatalf("no hub: %+v %v", rep, err)
	}

	// The hub reports but sent no ledger yet: Quibase only, noted.
	hubConnected(t, svc)
	if rep, _ = svc.DriverReport(ctx, juma.ID); rep.POS == nil || rep.POS.Checked || rep.POS.Note == "" {
		t.Fatalf("no ledger yet: %+v", rep.POS)
	}

	// A ledger with the check Quibase has and one the hub missed.
	if err := svc.SetDriverLedger(ctx, order.DriverLedger{OperationDay: "2026-10-26", GeneratedAt: time.Now(),
		Checks: []order.LedgerCheck{lcheck("1001", 99, "31"), lcheck("1002", 100, "31")}}); err != nil {
		t.Fatal(err)
	}
	rep, err = svc.DriverReport(ctx, juma.ID)
	if err != nil || !rep.POS.Checked || len(rep.Lines) != 2 || rep.Deliveries != 2 || rep.TipCents != 600 {
		t.Fatalf("merged: %+v %v", rep, err)
	}
	var only int
	for _, l := range rep.Lines {
		if l.Match == order.MatchPOSOnly && l.ExtRef == "1002" {
			only++
		}
	}
	if only != 1 {
		t.Fatalf("POS-only line missing: %+v", rep.Lines)
	}
	// The batch reports (Close batch / Batch tab) merge the same way.
	reps, err := svc.DriverReports(ctx, 0)
	if err != nil || len(reps) != 1 || len(reps[0].Lines) != 2 {
		t.Fatalf("batch reports: %+v %v", reps, err)
	}

	// Too old: not used any more.
	old := LedgerFreshFor
	LedgerFreshFor = time.Millisecond
	t.Cleanup(func() { LedgerFreshFor = old })
	time.Sleep(5 * time.Millisecond)
	if rep, _ = svc.DriverReport(ctx, juma.ID); rep.POS.Checked || len(rep.Lines) != 1 {
		t.Fatalf("stale: %+v", rep)
	}

	// Unknown driver id.
	if _, err := svc.DriverReport(ctx, "nope"); err != order.ErrDriverNotFound {
		t.Fatalf("unknown driver: %v", err)
	}
}

// The print asks the hub for a fresh ledger through the feed and waits for it.
func TestPrintDriverReportAsksForAFreshLedger(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	docs := &docLog{}
	docs.wire(svc)
	juma, _ := svc.CreateDriver(ctx, "Juma", "31")
	_, _ = svc.ClockIn(ctx, juma.ID)
	deliveredPOS(t, svc, "1001", "31", juma.ID)
	hubConnected(t, svc)
	_ = svc.SetDriverLedger(ctx, order.DriverLedger{Checks: []order.LedgerCheck{lcheck("1001", 99, "31")}})
	after := feedOf(t, svc)[len(feedOf(t, svc))-1].Seq

	// The "hub": answers driver_ledger.requested with a newer ledger.
	done := make(chan struct{})
	go func() {
		defer close(done)
		page, err := svc.Feed(ctx, after, 3*time.Second)
		if err != nil {
			return
		}
		for _, e := range page.Events {
			if e.Type == order.FeedDriverLedgerRequested {
				_ = svc.SetDriverLedger(ctx, order.DriverLedger{Checks: []order.LedgerCheck{lcheck("1001", 99, "31"), lcheck("1003", 101, "31")}})
				return
			}
		}
	}()
	start := time.Now()
	res, err := svc.PrintDriverReport(ctx, juma.ID, "button")
	<-done
	if err != nil || !res.Printed() || res.PrintedOn != "Front" || res.DriverName != "Juma" {
		t.Fatalf("print: %+v %v", res, err)
	}
	if time.Since(start) > LedgerWait {
		t.Fatalf("waited the whole %s for a ledger that came at once", LedgerWait)
	}
	slip := docs.last(t)
	if len(slip.Lines) != 2 || !slip.POS.Checked {
		t.Fatalf("slip = %+v", slip)
	}
	raw, _ := json.Marshal(slip)
	if !strings.Contains(string(raw), `"ext_ref":"1003"`) {
		t.Fatalf("the fresh ledger's check is not on the slip: %s", raw)
	}

	// Nothing printed: a result with the error (not an error).
	docs.fail = true
	if res, err = svc.PrintDriverReport(ctx, juma.ID, "button"); err != nil || res.Printed() || res.Error != "printer offline" {
		t.Fatalf("failed print: %+v %v", res, err)
	}
}

// With the hub gone quiet nothing is asked or waited for.
func TestPrintDriverReportNoHubNoWait(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	if _, err := svc.PrintDriverReport(ctx, "x", "button"); err != ErrPrintingUnavailable {
		t.Fatalf("not wired: %v", err)
	}
	docs := &docLog{}
	docs.wire(svc)
	sam, _ := svc.CreateDriver(ctx, "Sam", "")
	start := time.Now()
	res, err := svc.PrintDriverReport(ctx, sam.ID, "clock-out")
	if err != nil || !res.Printed() || time.Since(start) > time.Second {
		t.Fatalf("print: %+v %v", res, err)
	}
	if slip := docs.last(t); slip.POS != nil || slip.Deliveries != 0 || slip.DriverName != "Sam" {
		t.Fatalf("slip = %+v", slip)
	}
	for _, e := range feedOf(t, svc) {
		if e.Type == order.FeedDriverLedgerRequested {
			t.Fatal("asked a hub that isn't there")
		}
	}
	// A hub that reports but never sent a ledger (an older hub): not asked,
	// no 5 s wait on every slip.
	hubConnected(t, svc)
	start = time.Now()
	if res, err = svc.PrintDriverReport(ctx, sam.ID, "clock-out"); err != nil || !res.Printed() || time.Since(start) > time.Second {
		t.Fatalf("older hub: %+v %v", res, err)
	}
	if slip := docs.last(t); slip.POS == nil || slip.POS.Checked {
		t.Fatalf("older hub slip = %+v", slip.POS)
	}
	for _, e := range feedOf(t, svc) {
		if e.Type == order.FeedDriverLedgerRequested {
			t.Fatal("asked a hub that never sent a ledger")
		}
	}
	if _, err := svc.PrintDriverReport(ctx, "nope", "button"); err != order.ErrDriverNotFound {
		t.Fatalf("unknown driver: %v", err)
	}
}
