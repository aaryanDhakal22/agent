package order

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// The POS driver ledger (2.3.0). The store hub reads the POS's delivery
// checks of its operation day (check #, the driver on it, money, voided /
// closed, out time — no customer data) and PUTs them here, every ~15 s and
// right away when the feed asks for a fresh one (driver_ledger.requested).
// A driver report is then the POS's delivery checks for that driver merged
// with Quibase's own deliveries, so a check the hub missed (an update or a
// restart blip) is still on the slip. Kept in memory only (no SD writes).

// FeedDriverLedgerRequested asks the hub for a fresh driver ledger now (a
// driver report is about to print). Data: {at}.
const FeedDriverLedgerRequested = "driver_ledger.requested"

// MaxLedgerChecks bounds one ledger PUT.
const MaxLedgerChecks = 2000

// DriverLedger is PUT /api/integration/driver-ledger.
type DriverLedger struct {
	// OperationDay is the POS day the checks belong to ("2026-10-26").
	OperationDay string `json:"operation_day"`
	// DayStartedAt / DayEndedAt bound that day (ended nil while it is open);
	// a batch that doesn't overlap the day isn't checked against it.
	DayStartedAt *time.Time    `json:"day_started_at,omitempty"`
	DayEndedAt   *time.Time    `json:"day_ended_at,omitempty"`
	GeneratedAt  time.Time     `json:"generated_at"`
	Checks       []LedgerCheck `json:"checks"`
	// UnknownPosNumbers are Quibase drivers' numbers the POS has no
	// employee for (their POS deliveries can't be looked up).
	UnknownPosNumbers []string `json:"unknown_pos_numbers"`
}

// LedgerCheck is one POS delivery check of the day.
type LedgerCheck struct {
	Check  string `json:"check"`  // the POS check (= a POS order's ext_ref)
	Number int64  `json:"number"` // the printed check # staff see
	// DriverPosNumber is the driver on the check as his Driver # ("" = none).
	DriverPosNumber string     `json:"driver_pos_number,omitempty"`
	DriverName      string     `json:"driver_name,omitempty"` // the POS's first name (staff)
	Closed          bool       `json:"closed"`                // settled
	Voided          bool       `json:"voided"`
	CheckAt         time.Time  `json:"check_at"`
	OutAt           *time.Time `json:"out_at,omitempty"`
	// Money as the hub works it out for a POS order (paid, cash due net of
	// a cash tip, tip) plus the payments by kind and the check total.
	Paid         bool  `json:"paid"`
	CashDueCents int64 `json:"cash_due_cents"`
	TipCents     int64 `json:"tip_cents"`
	TotalCents   int64 `json:"total_cents"`
	CashCents    int64 `json:"cash_cents"`
	CardCents    int64 `json:"card_cents"`
}

// Validate checks a ledger PUT.
func (l *DriverLedger) Validate() error {
	if len(l.Checks) > MaxLedgerChecks {
		return fmt.Errorf("at most %d checks", MaxLedgerChecks)
	}
	seen := map[string]bool{}
	for i := range l.Checks {
		c := &l.Checks[i]
		c.Check = strings.TrimSpace(c.Check)
		c.DriverPosNumber = strings.TrimSpace(c.DriverPosNumber)
		if c.Check == "" || utf8.RuneCountInString(c.Check) > 64 {
			return errors.New("check must be 1-64 characters")
		}
		if seen[c.Check] {
			return fmt.Errorf("check %s is listed twice", c.Check)
		}
		seen[c.Check] = true
		if utf8.RuneCountInString(c.DriverPosNumber) > 12 || utf8.RuneCountInString(c.DriverName) > 40 {
			return errors.New("driver_pos_number is at most 12 and driver_name 40 characters")
		}
		if c.CashDueCents < 0 || c.TipCents < 0 || c.TotalCents < 0 || c.CashCents < 0 || c.CardCents < 0 {
			return errors.New("money can't be negative")
		}
	}
	if l.UnknownPosNumbers == nil {
		l.UnknownPosNumbers = []string{}
	}
	if l.Checks == nil {
		l.Checks = []LedgerCheck{}
	}
	return nil
}

// Covers reports whether the ledger's POS day overlaps [from, to] (to nil =
// still open). A ledger with no day bounds is taken to cover it.
func (l *DriverLedger) Covers(from time.Time, to *time.Time, now time.Time) bool {
	end := now
	if to != nil {
		end = *to
	}
	if l.DayStartedAt != nil && !end.After(*l.DayStartedAt) {
		return false
	}
	if l.DayEndedAt != nil && !from.Before(*l.DayEndedAt) {
		return false
	}
	return true
}

// How the POS ledger could be used for a report.
const (
	PosNone     = ""          // the store has no POS hub: reports as before
	PosOK       = "ok"        // merged
	PosStale    = "stale"     // the hub's last ledger is too old
	PosMissing  = "missing"   // a hub reports, but no ledger arrived yet
	PosOtherDay = "other_day" // the ledger is another POS day than the batch
)

// ReportPOS says whether a report was checked against the POS.
type ReportPOS struct {
	Checked bool       `json:"checked"`
	AsOf    *time.Time `json:"as_of,omitempty"` // when the ledger used arrived
	Note    string     `json:"note,omitempty"`  // why it wasn't checked
}

// Line match values (DriverReportLine.Match).
const (
	MatchBoth        = "both"         // on Quibase and in the POS
	MatchPOSOnly     = "pos_only"     // only the POS had it
	MatchQuibaseOnly = "quibase_only" // a POS order the POS doesn't list today
)

// MergeInput is everything MergeDriverReports needs.
type MergeInput struct {
	Reports []DriverReport // Quibase's (lines from NewDriverReportLine)
	Drivers []Driver       // the active roster (pos numbers)
	// POS are the batch's POS orders (any state) by ext_ref.
	POS map[string]StoredOrder
	// Batch window.
	BatchID       int64
	BatchOpenedAt time.Time
	BatchClosedAt *time.Time
	// Ledger and how it may be used (PosOK merges; anything else only notes).
	Ledger     *DriverLedger
	LedgerAsOf *time.Time
	State      string
	// StaleNote explains a stale / missing ledger on the slip.
	StaleNote string
}

// NormPosNumber compares driver numbers: trimmed, and digits without
// leading zeros ("031" = "31").
func NormPosNumber(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return s
		}
	}
	if t := strings.TrimLeft(s, "0"); t != "" {
		return t
	}
	return "0"
}

// MergeDriverReports merges the POS ledger into Quibase's driver reports:
//
//   - a POS order on a report is matched to its POS check: the POS's money
//     wins; the POS's driver decides whose report it is on (moved, with a
//     note on both); voided in the POS → off the report (noted); not listed
//     in the POS → kept, marked; no driver in the POS → kept, noted;
//   - a POS delivery check with a driver that no report has (the hub missed
//     it, or it never went out on Quibase) is added to that driver's report,
//     marked "POS only" — a driver Quibase doesn't know gets a slip of his
//     own;
//   - online orders follow Quibase, untouched;
//   - a driver with no Driver # (or one the POS doesn't know) is Quibase
//     data only, with a note.
//
// Totals are not filled (Finish). Reports come back sorted by name.
func MergeDriverReports(in MergeInput) []DriverReport {
	reps := make([]DriverReport, len(in.Reports))
	copy(reps, in.Reports)
	if in.State == PosNone {
		return reps
	}
	byID := map[string]Driver{}
	byPos := map[string]Driver{}
	for _, d := range in.Drivers {
		byID[d.ID] = d
		if p := NormPosNumber(d.PosNumber); p != "" {
			byPos[p] = d
		}
	}
	for i := range reps {
		if d, ok := byID[reps[i].DriverID]; ok && reps[i].DriverPosNumber == "" {
			reps[i].DriverPosNumber = d.PosNumber
		}
		reps[i].Lines = append([]DriverReportLine(nil), reps[i].Lines...)
		reps[i].Notes = append([]string(nil), reps[i].Notes...)
	}
	if in.State != PosOK || in.Ledger == nil {
		for i := range reps {
			reps[i].POS = &ReportPOS{AsOf: in.LedgerAsOf, Note: in.StaleNote}
		}
		return sortReports(reps)
	}

	led := in.Ledger
	unknown := map[string]bool{}
	for _, p := range led.UnknownPosNumbers {
		unknown[NormPosNumber(p)] = true
	}
	checks := map[string]LedgerCheck{}
	for _, c := range led.Checks {
		checks[c.Check] = c
	}
	// reportFor finds (or adds) the report of the driver with POS number pos.
	reportFor := func(pos, posName string) int {
		n := NormPosNumber(pos)
		for i := range reps {
			if NormPosNumber(reps[i].DriverPosNumber) == n {
				return i
			}
		}
		r := DriverReport{BatchID: in.BatchID, BatchOpenedAt: in.BatchOpenedAt, BatchClosedAt: in.BatchClosedAt,
			DriverPosNumber: strings.TrimSpace(pos), Lines: []DriverReportLine{}}
		if d, ok := byPos[n]; ok {
			r.DriverID, r.DriverName, r.DriverPosNumber = d.ID, d.Name, d.PosNumber
		} else {
			r.DriverName = strings.TrimSpace(posName)
			if r.DriverName == "" {
				r.DriverName = "POS #" + strings.TrimSpace(pos)
			}
		}
		reps = append(reps, r)
		return len(reps) - 1
	}
	nameOf := func(pos, posName string) string {
		if d, ok := byPos[NormPosNumber(pos)]; ok {
			return d.Name
		}
		if s := strings.TrimSpace(posName); s != "" {
			return s
		}
		return "POS #" + strings.TrimSpace(pos)
	}

	type move struct {
		pos, posName string
		line         DriverReportLine
	}
	var moves []move
	used := map[string]bool{}
	for i := range reps {
		r := &reps[i]
		kept := make([]DriverReportLine, 0, len(r.Lines))
		for _, l := range r.Lines {
			if l.ExtRef == "" { // online: Quibase decides
				kept = append(kept, l)
				continue
			}
			c, ok := checks[l.ExtRef]
			switch {
			case !ok:
				l.Match, l.Note = MatchQuibaseOnly, "not found in POS"
				kept = append(kept, l)
			case c.Voided:
				used[c.Check] = true
				r.Notes = append(r.Notes, fmt.Sprintf("%s voided in POS - not counted", lineNumber(l)))
			default:
				used[c.Check] = true
				l = l.withPOSMoney(c)
				l.Match = MatchBoth
				switch {
				case c.DriverPosNumber == "":
					l.Note = "no driver in POS"
					kept = append(kept, l)
				case NormPosNumber(c.DriverPosNumber) == NormPosNumber(r.DriverPosNumber):
					kept = append(kept, l)
				default: // the POS's driver wins for a terminal check
					to := nameOf(c.DriverPosNumber, c.DriverName)
					r.Notes = append(r.Notes, fmt.Sprintf("%s is on %s's report (POS)", lineNumber(l), to))
					l.Note = "Quibase had " + r.DriverName
					moves = append(moves, move{c.DriverPosNumber, c.DriverName, l})
				}
			}
		}
		r.Lines = kept
	}
	for _, m := range moves {
		i := reportFor(m.pos, m.posName)
		reps[i].Lines = append(reps[i].Lines, m.line)
	}

	// POS delivery checks no report had.
	for _, c := range led.Checks {
		if used[c.Check] || c.Voided || c.DriverPosNumber == "" {
			continue
		}
		so, held := in.POS[c.Check]
		if !held && !inWindow(c.CheckAt, in.BatchOpenedAt, in.BatchClosedAt) {
			continue // another batch's check
		}
		n := c.Number
		l := DriverReportLine{ExtRef: c.Check, DisplayNumber: &n, Source: "pos", Customer: "POS check",
			OutAt: c.OutAt, Match: MatchPOSOnly, Note: "POS only"}
		if held {
			x := NewDriverReportLine(so)
			l.OrderID, l.Customer, l.Address, l.Street = x.OrderID, x.Customer, x.Address, x.Street
			switch {
			case so.Status.State == StateOnDelivery && c.Closed:
				// Sent out from the terminal and back there (closed): the
				// return is the POS's, Quibase just never heard it.
				l.Match, l.Note = MatchBoth, ""
			case so.Status.State == StateOnDelivery:
				l.Match, l.Note = MatchBoth, "still out on Quibase"
			case so.Status.CameBack || so.Status.NotDelivered != nil:
				l.Note = "came back on Quibase"
			default:
				l.Note = "not sent out on Quibase"
			}
		}
		l = l.withPOSMoney(c)
		i := reportFor(c.DriverPosNumber, c.DriverName)
		reps[i].Lines = append(reps[i].Lines, l)
	}

	asOf := in.LedgerAsOf
	for i := range reps {
		r := &reps[i]
		pos := NormPosNumber(r.DriverPosNumber)
		switch {
		case r.DriverID == "":
			r.POS = &ReportPOS{Checked: true, AsOf: asOf}
			r.Notes = append([]string{fmt.Sprintf("Not a Quibase driver (POS #%s) - POS data only", r.DriverPosNumber)}, r.Notes...)
		case pos == "":
			r.POS = &ReportPOS{AsOf: asOf, Note: "No Driver # on Quibase"}
		case unknown[pos]:
			r.POS = &ReportPOS{AsOf: asOf, Note: fmt.Sprintf("Driver #%s is not in the POS", r.DriverPosNumber)}
		default:
			r.POS = &ReportPOS{Checked: true, AsOf: asOf}
		}
		sortLines(r.Lines)
	}
	return sortReports(reps)
}

// withPOSMoney takes the POS's money for a line (the POS order rule: card-
// paid → the tip counts; otherwise cash due, already net of a cash tip).
func (l DriverReportLine) withPOSMoney(c LedgerCheck) DriverReportLine {
	l.TotalCents = c.TotalCents
	l.Cash, l.CashCents, l.TipCents = posMoney(c.Paid, c.CashDueCents, c.TipCents)
	l.setSlipMoney(c.TotalCents, c.TipCents) // the POS's total has no tip in it
	if l.OutAt == nil {
		l.OutAt = c.OutAt
	}
	return l
}

// posMoney is the POS order money rule (NewDriverReportLine).
func posMoney(paid bool, cashDue, tip int64) (cash bool, cashCents, tipCents int64) {
	if cashDue > 0 || !paid {
		return true, cashDue, 0
	}
	return false, 0, tip
}

func inWindow(at, from time.Time, to *time.Time) bool {
	if at.IsZero() {
		return true
	}
	if at.Before(from) {
		return false
	}
	return to == nil || !at.After(*to)
}

// lineNumber is how a slip names a delivery ("#57").
func lineNumber(l DriverReportLine) string {
	switch {
	case l.KitchenNumber != nil:
		return fmt.Sprintf("#%d", *l.KitchenNumber)
	case l.DisplayNumber != nil:
		return fmt.Sprintf("#%d", *l.DisplayNumber)
	case l.ExtRef != "":
		return "check " + l.ExtRef
	}
	return "an order"
}

// sortLines orders a report's deliveries by time (delivered, else out);
// lines with neither go last.
func sortLines(ls []DriverReportLine) {
	at := func(l DriverReportLine) *time.Time {
		if l.DeliveredAt != nil {
			return l.DeliveredAt
		}
		return l.OutAt
	}
	sort.SliceStable(ls, func(i, j int) bool {
		a, b := at(ls[i]), at(ls[j])
		switch {
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return a.Before(*b)
	})
}

func sortReports(out []DriverReport) []DriverReport {
	sort.SliceStable(out, func(i, j int) bool {
		if !strings.EqualFold(out[i].DriverName, out[j].DriverName) {
			return strings.ToLower(out[i].DriverName) < strings.ToLower(out[j].DriverName)
		}
		return out[i].DriverID < out[j].DriverID
	})
	return out
}
