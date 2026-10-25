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
			{KitchenNumber: &k1, Customer: "Erik D.", Address: "12 Main St, Apt 3", Cash: true, CashCents: 2249},
			{KitchenNumber: &k2, Customer: "Ana P.", Address: "4 Elm St", Cash: true, CashCents: 1850},
			{Customer: "Lee H.", Address: "9 Oak Ave", TipCents: 400},
			{DisplayNumber: &pos, Source: order.SourcePOS, Customer: "Customer", TipCents: 500}, // POS card, PII wiped
		}}
	rep.Finish(600)
	sc := storeconfig.StoreContext{Location: storeconfig.LocationContext{Name: "Lombardi's Pizza", Timezone: "America/New_York"}}
	return rep, sc
}

var printed = time.Date(2026, 10, 17, 3, 50, 0, 0, time.UTC)

func TestBuildDriverReport_Golden(t *testing.T) {
	rep, sc := reportFixture()
	got := BuildDriverReport(rep, sc, "", printed)
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
	s := string(BuildDriverReport(rep, sc, "Online", printed))
	for _, want := range []string{
		" REROUTED from ONLINE ",
		"ABDUL",
		"Batch #3",
		"Oct 16 10:02 AM - 11:48 PM", // New York time
		"#1104 Erik D.",
		"CASH $22.49",
		"  12 Main St, Apt 3",
		"  Tip $4.00",
		"Lee H.",
		"CARD",
		"#57 Customer",
		leftRight("Deliveries", "4", lineWidth),
		leftRight("Total cash owed", "$40.99", lineWidth),
		leftRight("Total tip (A/T)", "$8.46", lineWidth),
		"($9.00 - 6%)",
		"OWED TO STORE",
		"$32.53",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(s, "  Tip $0.00") {
		t.Error("a cash order's tip is not listed (2.1.0)")
	}
	if strings.Contains(s, "$30.00") {
		t.Error("a card order's amount must not be printed")
	}
}

func TestBuildDriverReport_StoreOwesAndEmpty(t *testing.T) {
	rep := order.DriverReport{BatchID: 1, BatchOpenedAt: printed, DriverName: "Carlos",
		Lines: []order.DriverReportLine{{Customer: "Sam T.", TipCents: 1000}}} // card only
	rep.Finish(600)
	s := string(BuildDriverReport(rep, storeconfig.StoreContext{}, "", printed))
	if !strings.Contains(s, "STORE OWES DRIVER") || !strings.Contains(s, "$9.40") || strings.Contains(s, "-$") {
		t.Errorf("store-owes slip wrong:\n%s", s)
	}
	empty := order.DriverReport{BatchID: 1, BatchOpenedAt: printed, DriverName: "Deep", Lines: []order.DriverReportLine{}}
	empty.Finish(600)
	s = string(BuildDriverReport(empty, storeconfig.StoreContext{}, "", printed))
	for _, want := range []string{"No deliveries this batch", leftRight("Deliveries", "0", lineWidth), "OWED TO STORE", "$0.00", " - open"} {
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
