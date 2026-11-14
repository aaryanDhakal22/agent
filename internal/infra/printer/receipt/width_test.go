package receipt

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// Every slip is laid out for the printer it prints on (contract/agent.md §2.3c "Paper width"): goldens per
// slip type at 48 / 42 / 32 columns in testdata/widths/ (-update). The print torture (tests/printcheck)
// checks the same slips dot for dot on 576 / 512 / 384-dot paper.

func TestNormalizeCols(t *testing.T) {
	for in, want := range map[int]int{0: 48, -3: 48, 48: 48, 64: 48, 47: 42, 42: 42, 41: 32, 32: 32, 20: 32} {
		if got := NormalizeCols(in); got != want {
			t.Errorf("NormalizeCols(%d) = %d, want %d", in, got, want)
		}
	}
	for cols, dots := range map[int]int{48: 576, 42: 512, 32: 384, 0: 576} {
		if got := DotsOf(cols); got != dots {
			t.Errorf("DotsOf(%d) = %d, want %d", cols, got, dots)
		}
	}
	l := layoutFor(Cols72)
	if l.big() != 21 || l.xl() != 14 || l.huge() != 10 || l.fontB() != 56 || l.nameW() != 30 {
		t.Errorf("42 columns: big %d xl %d huge %d fontB %d name %d", l.big(), l.xl(), l.huge(), l.fontB(), l.nameW())
	}
}

// The width test's ruler: the last symbol on the first printed line names the width (the terminal's read-off).
func TestWidthRuler(t *testing.T) {
	r := WidthRuler()
	if len(r) != RulerLength || !strings.HasPrefix(r, strings.Repeat("-", 30)+"AB") || !strings.HasSuffix(r, "89") {
		t.Fatalf("ruler %q", r)
	}
	for sym, cols := range map[string]int{"B": 32, "L": 42, "r": 48, "9": 64, "A": 31} {
		if got, ok := ColumnsOfSymbol(sym); !ok || got != cols {
			t.Errorf("ColumnsOfSymbol(%q) = %d %v, want %d", sym, got, ok, cols)
		}
		if r[cols-1:cols] != strings.ToUpper(sym) {
			t.Errorf("ruler column %d is %q, want %q", cols, r[cols-1:cols], sym)
		}
		if SymbolOfColumns(cols) != strings.ToUpper(sym) {
			t.Errorf("SymbolOfColumns(%d) = %q", cols, SymbolOfColumns(cols))
		}
	}
	for _, bad := range []string{"0", "1", "", "AB", "?"} {
		if _, ok := ColumnsOfSymbol(bad); ok {
			t.Errorf("ColumnsOfSymbol(%q) accepted", bad)
		}
	}
	slip := string(BuildWidthTest("Online", time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)))
	if !strings.Contains(slip, "\n"+r+"\n") || !strings.Contains(slip, "Which letter is LAST") {
		t.Fatalf("width test slip %q", slip)
	}
	for _, line := range strings.Split(slip, "\n") {
		if line != r && len(stripESC(line)) > Cols58 {
			t.Errorf("width test line %q is wider than 32 columns", line)
		}
	}
}

// stripESC drops the ESC / GS commands this package writes (for a rough length check).
func stripESC(s string) string {
	for _, c := range []string{cmdInit, cmdCenter, cmdLeft, cmdBoldOn, cmdBoldOff, cmdNormalSz, cmdBaseSz, cmdCut} {
		s = strings.ReplaceAll(s, c, "")
	}
	return s
}

// A ticket meant for an 80 mm station that lands on the 42-column Online printer is laid out for 42: the
// bands 21 wide (not 24), the rules 42, the item list on 42.
func TestReroutedToNarrowerPrinter(t *testing.T) {
	snap, tk := stationFixture()
	wide := string(Build(snap, previewStore(), tk, order.DefaultReceiptPrefs()))
	tk.Cols = Cols72
	narrow := string(Build(snap, previewStore(), tk, order.DefaultReceiptPrefs()))
	for _, want := range []string{cmdInvertOn + pad("REROUTED FROM OVEN", 21) + cmdInvertOff, strings.Repeat("-", 42) + "\n"} {
		if !strings.Contains(narrow, want) {
			t.Errorf("42 columns: missing %q", want)
		}
	}
	if strings.Contains(narrow, strings.Repeat("-", 43)) || strings.Contains(narrow, pad("REROUTED FROM OVEN", 24)) {
		t.Error("42 columns: an 80 mm rule or band left")
	}
	if !strings.Contains(wide, cmdInvertOn+pad("REROUTED FROM OVEN", 24)+cmdInvertOff) {
		t.Error("48 columns: band not 24 wide")
	}
	rc := receiptTicket()
	rc.Cols, rc.ReroutedFrom = Cols72, "Pizza"
	r := string(Build(previewDelivery(), previewStore(), rc, order.DefaultReceiptPrefs()))
	if !strings.Contains(r, fmt.Sprintf("%-3s %-30s %7s", "Qty", "Item", "Price")) {
		t.Errorf("42-column receipt: the item list header isn't on 42 columns")
	}
}

// The powered-by logo is as wide as the printer: 576 dots on 80 mm, the 80 mm logo's middle 512 on a
// 72 mm print area, the 58 mm logo (384) on 58 mm.
func TestPoweredByFooter_Widths(t *testing.T) {
	for cols, wb := range map[int]int{Cols80: 72, Cols72: 64, Cols58: 48} {
		f := []byte(poweredByFooter(layoutFor(cols)))
		i := bytes.Index(f, []byte{0x1d, 0x76, 0x30, 0x00})
		if i < 0 {
			t.Fatalf("%d: no raster", cols)
		}
		if got := int(f[i+4]) + int(f[i+5])<<8; got != wb {
			t.Errorf("%d columns: raster %d bytes wide, want %d", cols, got, wb)
		}
	}
}

func TestWidthGoldens(t *testing.T) {
	sc := previewStore()
	at := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	prevNow := deliveryNow
	deliveryNow = func() time.Time { return at }
	defer func() { deliveryNow = prevNow }()
	for _, cols := range []int{Cols80, Cols72, Cols58} {
		on := printer.Paper{Cols: cols}
		rerouted := printer.Paper{Cols: cols, ReroutedFrom: "Pizza"}
		snap, st := stationFixture()
		st.Cols = cols
		change := st
		change.Change, change.Removed = true, receiptFixture().Items[:1]
		rep, rsc := reportFixture()
		k := int64(1104)
		un := order.UnassignedReport{BatchID: 3, BatchOpenedAt: at.Add(-8 * time.Hour), CashCents: 2249, TipCents: 200,
			Lines: []order.UnassignedLine{{DriverReportLine: order.DriverReportLine{KitchenNumber: &k, Street: "12 Main St",
				Cash: true, SlipTotalCents: 2249, OrderTipCents: 200}, MarkedAt: at, MarkedBy: order.MarkedBy{Name: "Ana"},
				FromDriver: "Abdul", Available: []string{"Abdul", "Ana"}}}}
		slips := map[string][]byte{
			"receipt_delivery":    Build(previewDelivery(), sc, printer.Ticket{Kind: printer.TicketReceipt, Cols: cols}, order.DefaultReceiptPrefs()),
			"receipt_rerouted":    Build(previewPickup(), sc, printer.Ticket{Kind: printer.TicketReceipt, Cols: cols, ReroutedFrom: "Pizza"}, order.DefaultReceiptPrefs()),
			"station":             Build(snap, sc, st, order.DefaultReceiptPrefs()),
			"station_change":      Build(snap, sc, change, order.DefaultReceiptPrefs()),
			"delivery_copies":     BuildDeliveryCopies(deliverySnap(false), sc, order.DefaultReceiptPrefs(), on),
			"driver_report":       BuildDriverReport(rep, rsc, "", rerouted, printed),
			"driver_report_small": BuildDriverReport(rep, rsc, order.SizeSmall, on, printed),
			"unassigned":          BuildUnassignedReport(un, sc, "", rerouted, at),
			"unsettled":           BuildUnsettledList(unsettledFixture(), sc, "", rerouted, at),
			"not_picked_up":       BuildNotPickedUp(notPickedUpFixture(), sc, "", rerouted, at),
			"test_slip":           BuildTestSlip("Pizza", sc, "Counter tablet", at, cols),
			"sample":              BuildSample(sc, order.DefaultReceiptPrefs(), at, cols),
		}
		for name, got := range slips {
			path := filepath.Join("testdata", "widths", fmt.Sprintf("%s_%d.golden", name, cols))
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s changed (run with -update after checking)", path)
			}
			// No separator wider than the printer.
			if strings.Contains(string(got), strings.Repeat("-", cols+1)) {
				t.Errorf("%s: a rule wider than %d columns", path, cols)
			}
		}
	}
}
