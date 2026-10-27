package receipt

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
)

// 2.2.3: a report merged with the POS driver ledger — line notes, report
// notes and the "POS checked" / "POS NOT CHECKED" footer.
func mergedReportFixture() order.DriverReport {
	rep, _ := reportFixture()
	n58, n61 := int64(58), int64(61)
	asOf := time.Date(2026, 10, 17, 3, 49, 0, 0, time.UTC)
	rep.Lines = append(rep.Lines,
		order.DriverReportLine{DisplayNumber: &n58, Source: order.SourcePOS, Customer: "POS check", Cash: true, CashCents: 1830,
			Match: order.MatchPOSOnly, Note: "POS only"},
		order.DriverReportLine{DisplayNumber: &n61, Source: order.SourcePOS, Customer: "Customer", TipCents: 200,
			Match: order.MatchBoth, Note: "Quibase had Ali"})
	rep.Notes = []string{"#62 is on Bob's report (POS)", "#63 voided in POS - not counted"}
	rep.POS = &order.ReportPOS{Checked: true, AsOf: &asOf}
	rep.Finish(600)
	return rep
}

func TestBuildDriverReport_MergedGolden(t *testing.T) {
	_, sc := reportFixture()
	got := BuildDriverReport(mergedReportFixture(), sc, "", printed)
	path := "testdata/driver_report_pos.golden"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("merged driver report differs from %s (run with -update after checking)\n%s", path, got)
	}
	s := string(got)
	for _, want := range []string{"#58 POS check", "  * POS only", "  * Quibase had Ali", "* #62 is on Bob's report (POS)",
		"* #63 voided in POS - not counted", "POS checked 11:49 PM", leftRight("Deliveries", "6", lineWidth)} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestBuildDriverReport_POSNotChecked(t *testing.T) {
	rep, sc := reportFixture()
	rep.POS = &order.ReportPOS{Note: "No Driver # on Quibase"}
	s := string(BuildDriverReport(rep, sc, "", printed))
	if !strings.Contains(s, "POS NOT CHECKED") || !strings.Contains(s, "No Driver # on Quibase") || strings.Contains(s, "POS checked") {
		t.Errorf("not-checked footer:\n%s", s)
	}
	// No POS hub: nothing about the POS at all (the 2.1.0 slip, byte for byte).
	rep.POS = nil
	if s := string(BuildDriverReport(rep, sc, "", printed)); strings.Contains(s, "POS NOT") || strings.Contains(s, "POS checked") {
		t.Errorf("no hub, no POS line:\n%s", s)
	}
}
