package order

import (
	"strings"
	"testing"
	"time"
)

// 2.3.0: driver reports merged with the POS driver ledger.

var (
	mOpened = time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	mAsOf   = time.Date(2026, 10, 26, 23, 0, 0, 0, time.UTC)
	juma    = Driver{ID: "d-juma", Name: "Juma", PosNumber: "31"}
	ali     = Driver{ID: "d-ali", Name: "Ali", PosNumber: "32"}
	noNum   = Driver{ID: "d-sam", Name: "Sam"}
)

func at(min int) *time.Time {
	t := mOpened.Add(time.Duration(min) * time.Minute)
	return &t
}

// onlineLine is an online delivery on a Quibase report (cash $20).
func onlineLine(id string, k int64, min int) DriverReportLine {
	return DriverReportLine{OrderID: id, KitchenNumber: &k, Source: "online", Customer: "Erik D.", Cash: true,
		CashCents: 2000, TotalCents: 2000, DeliveredAt: at(min)}
}

// posLine is a POS delivery on a Quibase report (cash $15 as the Pi saw it).
func posLine(id, check string, n int64, min int) DriverReportLine {
	return DriverReportLine{OrderID: id, DisplayNumber: &n, Source: SourcePOS, ExtRef: check, Customer: "Pat D.",
		Cash: true, CashCents: 1500, TotalCents: 1500, DeliveredAt: at(min)}
}

func report(d Driver, lines ...DriverReportLine) DriverReport {
	return DriverReport{BatchID: 7, BatchOpenedAt: mOpened, DriverID: d.ID, DriverName: d.Name, Lines: lines}
}

// ledgerCheck: a closed card check with a $3 tip, unless changed.
func ledgerCheck(check string, n int64, pos string, min int) LedgerCheck {
	return LedgerCheck{Check: check, Number: n, DriverPosNumber: pos, Closed: true, CheckAt: *at(min - 20), OutAt: at(min),
		Paid: true, TipCents: 300, TotalCents: 2400, CardCents: 2700}
}

func merge(reps []DriverReport, pos map[string]StoredOrder, checks ...LedgerCheck) []DriverReport {
	asOf := mAsOf
	return MergeDriverReports(MergeInput{Reports: reps, Drivers: []Driver{juma, ali, noNum}, POS: pos, BatchID: 7,
		BatchOpenedAt: mOpened, Ledger: &DriverLedger{OperationDay: "2026-10-26", Checks: checks}, LedgerAsOf: &asOf, State: PosOK})
}

func byName(t *testing.T, reps []DriverReport, name string) DriverReport {
	t.Helper()
	for _, r := range reps {
		if r.DriverName == name {
			return r
		}
	}
	t.Fatalf("no report for %s in %+v", name, reps)
	return DriverReport{}
}

func TestMergeOnlineOnly(t *testing.T) {
	reps := merge([]DriverReport{report(juma, onlineLine("o1", 1101, 30))}, nil)
	r := byName(t, reps, "Juma")
	if len(r.Lines) != 1 || r.Lines[0].Match != "" || r.Lines[0].Note != "" || r.Lines[0].CashCents != 2000 {
		t.Fatalf("online lines follow Quibase untouched: %+v", r.Lines)
	}
	if r.POS == nil || !r.POS.Checked || r.POS.AsOf == nil || !r.POS.AsOf.Equal(mAsOf) || len(r.Notes) != 0 {
		t.Fatalf("pos = %+v notes %v", r.POS, r.Notes)
	}
}

func TestMergePOSOnlyIsAdded(t *testing.T) {
	// The hub missed check 9001 (#57): only the POS has it, on Juma.
	reps := merge([]DriverReport{report(juma, onlineLine("o1", 1101, 30))}, nil, ledgerCheck("9001", 57, "31", 45))
	r := byName(t, reps, "Juma")
	if len(r.Lines) != 2 {
		t.Fatalf("lines = %+v", r.Lines)
	}
	l := r.Lines[1]
	if l.ExtRef != "9001" || *l.DisplayNumber != 57 || l.Match != MatchPOSOnly || l.Note != "POS only" ||
		l.Cash || l.TipCents != 300 || l.TotalCents != 2400 || l.Customer != "POS check" {
		t.Fatalf("POS-only line = %+v", l)
	}
	r.Finish(600)
	if r.Deliveries != 2 || r.CashCents != 2000 || r.TipCents != 300 {
		t.Fatalf("totals = %+v", r)
	}
}

func TestMergeBothUsesPOSMoney(t *testing.T) {
	// On both sides: the POS's money wins (settled by card since the pull).
	reps := merge([]DriverReport{report(juma, posLine("p1", "9002", 58, 40))}, nil, ledgerCheck("9002", 58, "031", 40))
	r := byName(t, reps, "Juma")
	if len(r.Lines) != 1 || r.Lines[0].Match != MatchBoth || r.Lines[0].Note != "" || r.Lines[0].Cash || r.Lines[0].TipCents != 300 {
		t.Fatalf("both = %+v", r.Lines)
	}
	// A cash check: cash due net of a cash tip, no tip counted.
	c := ledgerCheck("9002", 58, "31", 40)
	c.Paid, c.CashDueCents, c.TipCents = false, 2100, 0
	reps = merge([]DriverReport{report(juma, posLine("p1", "9002", 58, 40))}, nil, c)
	if l := byName(t, reps, "Juma").Lines[0]; !l.Cash || l.CashCents != 2100 || l.TipCents != 0 {
		t.Fatalf("cash = %+v", l)
	}
}

func TestMergeConflictPOSDriverWins(t *testing.T) {
	// Quibase sent #59 out with Ali, the terminal says Juma: Juma's report.
	reps := merge([]DriverReport{
		report(juma, onlineLine("o1", 1101, 30)),
		report(ali, posLine("p3", "9003", 59, 35), onlineLine("o2", 1102, 50)),
	}, nil, ledgerCheck("9003", 59, "31", 35))
	j, a := byName(t, reps, "Juma"), byName(t, reps, "Ali")
	if len(j.Lines) != 2 || j.Lines[1].ExtRef != "9003" || j.Lines[1].Note != "Quibase had Ali" || j.Lines[1].Match != MatchBoth {
		t.Fatalf("Juma = %+v", j.Lines)
	}
	if len(a.Lines) != 1 || a.Lines[0].OrderID != "o2" || len(a.Notes) != 1 || a.Notes[0] != "#59 is on Juma's report (POS)" {
		t.Fatalf("Ali = %+v %v", a.Lines, a.Notes)
	}
	// Online orders follow Quibase whatever the POS says about anything.
	if a.Lines[0].Match != "" {
		t.Fatalf("online line marked: %+v", a.Lines[0])
	}
}

func TestMergeVoidedExcluded(t *testing.T) {
	v := ledgerCheck("9004", 60, "31", 40)
	v.Voided = true
	w := ledgerCheck("9005", 61, "31", 50)
	w.Voided = true
	reps := merge([]DriverReport{report(juma, posLine("p4", "9004", 60, 40))}, nil, v, w)
	r := byName(t, reps, "Juma")
	if len(r.Lines) != 0 || len(r.Notes) != 1 || r.Notes[0] != "#60 voided in POS - not counted" {
		t.Fatalf("voided: %+v %v", r.Lines, r.Notes)
	}
}

func TestMergeNotInPOSAndNoPOSDriver(t *testing.T) {
	c := ledgerCheck("9007", 63, "", 40) // the POS shows no driver
	reps := merge([]DriverReport{report(juma, posLine("p6", "9006", 62, 30), posLine("p7", "9007", 63, 40))}, nil, c)
	r := byName(t, reps, "Juma")
	if len(r.Lines) != 2 || r.Lines[0].Match != MatchQuibaseOnly || r.Lines[0].Note != "not found in POS" ||
		r.Lines[1].Match != MatchBoth || r.Lines[1].Note != "no driver in POS" {
		t.Fatalf("lines = %+v", r.Lines)
	}
}

func TestMergeUnknownPOSDriverGetsASlip(t *testing.T) {
	c := ledgerCheck("9008", 64, "40", 40)
	c.DriverName = "Rafi"
	reps := merge([]DriverReport{report(juma)}, nil, c)
	r := byName(t, reps, "Rafi")
	if r.DriverID != "" || r.DriverPosNumber != "40" || len(r.Lines) != 1 || r.POS == nil || !r.POS.Checked ||
		len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "Not a Quibase driver (POS #40)") {
		t.Fatalf("POS driver report = %+v", r)
	}
}

func TestMergeDriverWithoutNumberOrUnknown(t *testing.T) {
	asOf := mAsOf
	reps := MergeDriverReports(MergeInput{
		Reports: []DriverReport{report(noNum, onlineLine("o1", 1101, 30)), report(ali, onlineLine("o2", 1102, 30))},
		Drivers: []Driver{juma, ali, noNum}, BatchID: 7, BatchOpenedAt: mOpened, LedgerAsOf: &asOf, State: PosOK,
		Ledger: &DriverLedger{UnknownPosNumbers: []string{"32"}, Checks: []LedgerCheck{ledgerCheck("9009", 65, "31", 40)}},
	})
	if s := byName(t, reps, "Sam"); s.POS == nil || s.POS.Checked || s.POS.Note != "No Driver # on Quibase" || len(s.Lines) != 1 {
		t.Fatalf("Sam = %+v", s)
	}
	if a := byName(t, reps, "Ali"); a.POS == nil || a.POS.Checked || a.POS.Note != "Driver #32 is not in the POS" {
		t.Fatalf("Ali = %+v", a.POS)
	}
	// Juma had nothing on Quibase but the POS has one for him: he gets a report.
	if j := byName(t, reps, "Juma"); j.DriverID != "d-juma" || len(j.Lines) != 1 || !j.POS.Checked {
		t.Fatalf("Juma = %+v", j)
	}
}

func TestMergeStaleOrMissingIsQuibaseOnly(t *testing.T) {
	in := []DriverReport{report(juma, posLine("p1", "9002", 58, 40))}
	for _, state := range []string{PosStale, PosMissing, PosOtherDay} {
		reps := MergeDriverReports(MergeInput{Reports: in, Drivers: []Driver{juma}, State: state, StaleNote: "POS link down",
			Ledger: &DriverLedger{Checks: []LedgerCheck{ledgerCheck("9001", 57, "31", 45)}}})
		r := reps[0]
		if len(r.Lines) != 1 || r.Lines[0].Match != "" || r.Lines[0].CashCents != 1500 || r.POS == nil || r.POS.Checked || r.POS.Note != "POS link down" {
			t.Fatalf("%s: %+v", state, r)
		}
	}
	// No hub at all: the report as before, no POS block.
	reps := MergeDriverReports(MergeInput{Reports: in, State: PosNone})
	if reps[0].POS != nil || len(reps[0].Lines) != 1 {
		t.Fatalf("no hub: %+v", reps[0])
	}
}

func TestMergeWindowAndHeldOrders(t *testing.T) {
	before := ledgerCheck("8000", 10, "31", -120) // yesterday's batch, not on the Pi
	before.CheckAt = mOpened.Add(-3 * time.Hour)
	out := ledgerCheck("9010", 66, "31", 60) // Quibase has it out with Juma (not back yet)
	back := ledgerCheck("9011", 67, "31", 70)
	pos := map[string]StoredOrder{
		"9010": {Order: Snapshot{ID: "p10", Source: SourcePOS}, Status: Status{State: StateOnDelivery, ExtSource: ExtSourceSoftTouch, ExtRef: "9010"}},
		"9011": {Order: Snapshot{ID: "p11", Source: SourcePOS}, Status: Status{State: StateReady, CameBack: true, ExtSource: ExtSourceSoftTouch, ExtRef: "9011"}},
	}
	r := byName(t, merge([]DriverReport{report(juma)}, pos, before, out, back), "Juma")
	if len(r.Lines) != 2 || r.Lines[0].OrderID != "p10" || r.Lines[0].Note != "still out on Quibase" || r.Lines[0].Match != MatchBoth ||
		r.Lines[1].Note != "came back on Quibase" || r.Lines[1].Match != MatchPOSOnly {
		t.Fatalf("lines = %+v", r.Lines)
	}
}

func TestLedgerValidateAndCovers(t *testing.T) {
	l := DriverLedger{Checks: []LedgerCheck{{Check: " 1 "}, {Check: "1"}}}
	if err := l.Validate(); err == nil {
		t.Fatal("a check listed twice must fail")
	}
	l = DriverLedger{Checks: []LedgerCheck{{Check: "1", TipCents: -1}}}
	if err := l.Validate(); err == nil {
		t.Fatal("negative money must fail")
	}
	l = DriverLedger{}
	if err := l.Validate(); err != nil || l.Checks == nil || l.UnknownPosNumbers == nil {
		t.Fatalf("empty ledger: %v %+v", err, l)
	}
	start := mOpened.Add(-4 * time.Hour)
	end := mOpened.Add(-time.Hour)
	l = DriverLedger{DayStartedAt: &start, DayEndedAt: &end}
	if l.Covers(mOpened, nil, mAsOf) {
		t.Fatal("a POS day that ended before the batch opened doesn't cover it")
	}
	l.DayEndedAt = nil
	if !l.Covers(mOpened, nil, mAsOf) {
		t.Fatal("the open POS day covers the open batch")
	}
	if NormPosNumber(" 031 ") != "31" || NormPosNumber("000") != "0" || NormPosNumber("A12") != "A12" {
		t.Fatal("NormPosNumber")
	}
}
