package receipt

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/storeconfig"
)

func reportFixture() (order.DriverReport, storeconfig.StoreContext) {
	k1, k2, pos := int64(1104), int64(1107), int64(57)
	opened := time.Date(2026, 10, 16, 14, 2, 0, 0, time.UTC)
	closed := time.Date(2026, 10, 17, 3, 48, 0, 0, time.UTC)
	rep := order.DriverReport{BatchID: 3, BatchOpenedAt: opened, BatchClosedAt: &closed, DriverName: "Abdul",
		Lines: []order.DriverReportLine{
			{KitchenNumber: &k1, Customer: "Erik D.", Address: "12 Main St, Apt 3", Street: "12 Main St", Cash: true, CashCents: 2249,
				OrderTipCents: 200, SlipTotalCents: 2249},
			{KitchenNumber: &k2, Customer: "Ana P.", Address: "4 Elm St", Street: "4 Elm St", Cash: true, CashCents: 1850, SlipTotalCents: 1850},
			{Customer: "Lee H.", Address: "9 Oak Ave", Street: "9 Oak Ave", TipCents: 400, OrderTipCents: 400, SlipTotalCents: 3000},
			{DisplayNumber: &pos, Source: order.SourcePOS, Customer: "Customer", TipCents: 500, OrderTipCents: 500, SlipTotalCents: 2900}, // POS card, PII wiped
		}}
	rep.Finish(600)
	sc := storeconfig.StoreContext{Location: storeconfig.LocationContext{Name: "Lombardi's Pizza", Timezone: "America/New_York"}}
	return rep, sc
}

var printed = time.Date(2026, 10, 17, 3, 50, 0, 0, time.UTC)

func TestBuildDriverReport_Golden(t *testing.T) {
	rep, sc := reportFixture()
	got := BuildDriverReport(rep, sc, "", "", printed)
	path := "testdata/driver_report.golden"
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
		t.Errorf("driver report differs from %s (run with -update after checking)\n%s", path, got)
	}
}

func TestBuildDriverReport_Content(t *testing.T) {
	rep, sc := reportFixture()
	s := string(BuildDriverReport(rep, sc, "", "Online", printed))
	const W = stationWidth // font A, the full 80 mm line
	for _, want := range []string{
		" REROUTED from ONLINE ",
		"ABDUL",
		"Batch #3",
		"Oct 16 10:02 AM - 11:48 PM", // New York time
		"CASH ORDERS (2)",
		"1104 12 Main St" + cmdBoldOff + "\n", // no "#", street only (no apt)
		leftRight("Tip: $2.00", "Total: $22.49", W),
		"1107 4 Elm St",
		leftRight("Tip: $0.00", "Total: $18.50", W),
		leftRight("Cash subtotal", "$40.99", W),
		"CARD ORDERS (2)",
		"---- 9 Oak Ave",
		leftRight("Tip: $4.00", "Total: $30.00", W),
		"57" + cmdBoldOff, // a POS check, no address
		leftRight("Tip: $5.00", "Total: $29.00", W),
		leftRight("Card tips", "$9.00", W),
		leftRight("Deliveries", "4", W),
		leftRight("Total cash owed", "$40.99", W),
		leftRight("Total tip (A/T)", "$8.46", W),
		"($9.00 - 6%)",
		leftRight("OWED TO STORE", "$33.00", W), // $32.53 rounded
		rightAlign("exact $32.53", W),
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	// No customer names, no "#" on ticket numbers, no apartment.
	for _, not := range []string{"Erik", "Ana P", "Lee H", "Customer", "#1104", "#57", "Apt 3"} {
		if strings.Contains(s, not) {
			t.Errorf("the slip must not print %q", not)
		}
	}
	// Cash orders come first, card orders below.
	if strings.Index(s, "1107 4 Elm St") > strings.Index(s, "9 Oak Ave") || strings.Index(s, "CASH ORDERS") > strings.Index(s, "CARD ORDERS") {
		t.Error("cash orders must print above card orders")
	}
	// No large text anywhere: no GS ! size and no double-height ESC ! at the
	// default size (normal = font A single height).
	if strings.Contains(s, "\x1d\x21") || strings.Contains(s, cmdBaseSz) || !strings.Contains(s, cmdNormalSz) {
		t.Errorf("driver report must be one normal size: %q", s)
	}
	// Font B (small) uses its full 64-character line.
	if small := string(BuildDriverReport(rep, sc, "small", "", printed)); !strings.Contains(small, leftRight("Tip: $2.00", "Total: $22.49", 64)) {
		t.Error("small: tip and total at the far ends of a 64-character line")
	}
}

// The slip's text size setting.
func TestBuildDriverReport_Sizes(t *testing.T) {
	rep, sc := reportFixture()
	for size, cmd := range map[string]string{"small": cmdSmallSz, "normal": cmdNormalSz, "": cmdNormalSz, "large": cmdBaseSz} {
		s := string(BuildDriverReport(rep, sc, size, "", printed))
		if !strings.HasPrefix(s, cmdInit+cmd) || strings.Contains(s, "\x1d\x21") {
			t.Errorf("size %q: starts %q", size, s[:6])
		}
	}
}

func TestBuildDriverReport_StoreOwesAndEmpty(t *testing.T) {
	rep := order.DriverReport{BatchID: 1, BatchOpenedAt: printed, DriverName: "Carlos",
		Lines: []order.DriverReportLine{{Customer: "Sam T.", TipCents: 1000}}} // card only
	rep.Finish(600)
	s := string(BuildDriverReport(rep, storeconfig.StoreContext{}, "", "", printed))
	if !strings.Contains(s, leftRight("STORE OWES DRIVER", "$9.00", stationWidth)) || !strings.Contains(s, "exact $9.40") || strings.Contains(s, "-$") {
		t.Errorf("store-owes slip wrong:\n%s", s)
	}
	empty := order.DriverReport{BatchID: 1, BatchOpenedAt: printed, DriverName: "Deep", Lines: []order.DriverReportLine{}}
	empty.Finish(600)
	s = string(BuildDriverReport(empty, storeconfig.StoreContext{}, "", "", printed))
	for _, want := range []string{"No deliveries this batch", leftRight("Deliveries", "0", stationWidth), "OWED TO STORE", "$0.00", " - open"} {
		if !strings.Contains(s, want) {
			t.Errorf("empty slip missing %q", want)
		}
	}
}

func TestPercent(t *testing.T) {
	for bps, want := range map[int64]string{600: "6%", 650: "6.5%", 0: "0%", 725: "7.25%"} {
		if got := percent(bps); got != want {
			t.Errorf("percent(%d) = %q, want %q", bps, got, want)
		}
	}
}
