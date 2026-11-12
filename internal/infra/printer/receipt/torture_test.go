package receipt

// The print torture corpus (quicc-ops TESTING.md "Print torture", tests/printcheck): every slip the Pi
// prints, fed extreme strings, dumped as <name>.bin + <name>.expect.json for the verifiers. Off unless
// PRINT_TORTURE_OUT names a folder:
//
//	PRINT_TORTURE_OUT=/tmp/pt go test ./internal/infra/printer/receipt -run TestPrintTortureDump -count=1
//
// The expectations list what each slip must print in full (wrapped anywhere): every item name, size,
// modifier, note, the customer, the address, the reroute printer... Struck-through lines are pictures, so
// a struck-through line is read back from its picture by the verifier.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

// The corpus (synthetic people: 555 numbers, made-up streets).
const (
	tName120   = "Extra Large Hand-Tossed Brooklyn Style Supreme Meat Lovers Pizza with Garlic Butter Crust and Double Mozzarella Cheese Blend"
	tName60    = "Philly Cheesesteak Sub with Fried Onions Peppers and Mushrooms"
	tUnicode1  = "Jalapeño Poppers with Piña Colada Dip"
	tUnicode2  = "Crème Brûlée Cheesecake"
	tEmoji     = "Pizza 🍕 Special 🔥"
	tCustomer  = "Maximiliano Alejandro Fernández-Castellanos de la Vega Rodríguez"
	tStaff     = "Bartholomew Alexander Montgomery-Fitzgerald III"
	tPrinter   = "Upstairs Wood-Fired Brick Oven Pizza Station Printer (Back Kitchen)"
	tStreet    = "12345 North Charles Street Extension Service Road"
	tApt       = "Apartment 1204B, Building C, rear entrance, buzz #7 twice"
	tCity      = "Lutherville-Timonium"
	tCoupon    = "2 Large 1-Topping Pizzas + 20 Wings + 2 Liter Soda Family Night Special (Tue-Thu only)"
	tModLong   = "Light Sauce on the Left Side Only Please Not Too Much"
	tStoreName = "Lombardi's Original Brick Oven Pizzeria, Subs, Wings & Italian Kitchen of Towson"
)

var (
	tNote300    = strings.TrimSpace(strings.Repeat("Please cut into squares not triangles, the kids like it that way, and leave it on the porch. ", 4))[:300]
	tDirections = "Go past the gate, second building on the left behind the dumpster, take the stairs not the elevator, third door with the blue wreath, knock loudly the bell is broken, dog is friendly"
)

func tMods(n int) []order.LineItem {
	base := []string{"Extra Pepperoni", "Sausage", "Mushrooms", "Green Peppers", "Black Olives", "Onions", "Bacon",
		"Ham", "Pineapple", "Spinach", "Fresh Tomatoes", "Banana Peppers", tModLong, "Anchovies", "Ricotta",
		"Feta", "Jalapeños", "Roasted Red Peppers", "Artichoke Hearts", "Garlic", "Basil", "Meatballs",
		"Grilled Chicken", "Buffalo Chicken", "Philly Steak", "Broccoli"}
	var out []order.LineItem
	for i := 0; i < n; i++ {
		m := order.LineItem{Name: base[i%len(base)], Quantity: 1}
		if i%3 == 0 {
			m.UnitPrice = 150 + int64(i)*10
		}
		if i == 5 {
			m.Quantity = 3
		}
		out = append(out, m)
	}
	return out
}

func tHalves() []order.LineItem {
	return []order.LineItem{
		{Name: "Extra Cheese", Quantity: 1, UnitPrice: 200},
		{Name: "Pepperoni (1st half)", Portion: "first", Quantity: 1, UnitPrice: 100},
		{Name: tModLong + " (1st half)", Portion: "first", Quantity: 1},
		{Name: "Jalapeños (2nd half)", Portion: "second", Quantity: 1, UnitPrice: 100},
		{Name: "Hawaiian (2nd half)", Portion: "second", Mode: "specialty", Quantity: 1},
		{Name: "No Onions (2nd half)", Portion: "second", Mode: "no", Quantity: 1},
	}
}

func tItem(qty int, name, size string, cents int64, printerID string, kids ...order.LineItem) order.LineItem {
	return order.LineItem{Name: name, Size: size, Quantity: qty, UnitPrice: cents, Routing: order.RoutingMatched,
		PrinterID: printerID, Children: kids}
}

type tortureJob struct {
	Paper  int      `json:"paper"`
	Source string   `json:"source"`
	Kind   string   `json:"kind"`
	Texts  []string `json:"texts"`
	Totals []string `json:"totals,omitempty"`
}

// textsOf: what a receipt / ticket must print of its items.
func textsOf(items []order.LineItem, sizes bool) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Name)
		if sizes && strings.TrimSpace(it.Size) != "" {
			out = append(out, it.Size)
		}
		if strings.TrimSpace(it.Notes) != "" {
			out = append(out, it.Notes)
		}
		for _, c := range it.Children {
			out = append(out, strings.TrimSuffix(strings.TrimSuffix(c.Name, " (1st half)"), " (2nd half)"))
		}
	}
	return out
}

func tortureSC() storeconfig.StoreContext {
	return storeconfig.StoreContext{Location: storeconfig.LocationContext{Name: tStoreName, Timezone: "America/New_York",
		AddressLine1: "931 Taylor Avenue, Suite 100 (entrance on the side street)", AddressLine2: "Towson, MD 21286"}}
}

func tortureSnap(typ string, items ...order.LineItem) order.Snapshot {
	k := int64(1187)
	placed := time.Date(2026, 11, 12, 23, 41, 0, 0, time.UTC)
	s := order.Snapshot{ID: "o-torture", SourceEventID: "V-90001", KitchenNumber: &k, Source: "web",
		PlacedAt: placed, Customer: order.Customer{DisplayName: tCustomer, Phone: "4105550199"},
		Fulfillment: order.Fulfillment{Type: typ}, Notes: tNote300, Items: items,
		Money: order.Money{Subtotal: 999999, Tax: 59999, Tip: 12345, Total: 1072343,
			Fees:      []order.Fee{{Type: "delivery", Label: "Delivery fee (outside the 3 mile zone, evening rate)", Amount: 599}, {Type: "service", Label: "Service fee", Amount: 150}},
			Discounts: []order.Discount{{Type: "promo", Label: "Employee meal discount for the closing shift crew", Amount: 99999}}},
		Payments: []order.PaymentSummary{{Method: "card", AmountCents: 1072343, Last4: "4242", AuthCode: "AUTH-0099887766"}},
	}
	if typ == "delivery" {
		s.Fulfillment.Address = &order.Address{Line1: tStreet, Line2: tApt, City: tCity, PostalCode: "21093", Notes: tDirections}
	}
	if typ == "dine_in" {
		s.Source = order.SourceCounter
	}
	return s
}

func tortureItems() []order.LineItem {
	long := tItem(2, tName120, "Large 16\" (Family Size, 12 Slices)", 2499, "p-pizza", tMods(24)...)
	long.Notes = "Well done, cut in squares, extra crispy on the bottom, sauce on the side, oregano on top"
	return []order.LineItem{
		long,
		tItem(99, tName60, "12\"", 999999, "p-sub", tMods(3)...),
		tItem(1, tUnicode1, "", 899, "p-fry", order.LineItem{Name: "Salsa Picante (añejo)", Quantity: 1}),
		tItem(1, tUnicode2, "Slice", 650, "p-pizza"),
		tItem(1, tEmoji, "Medium", 1599, "p-pizza", order.LineItem{Name: "🌶 Extra Hot", Quantity: 1}),
		tItem(1, "Q", "", 100, "p-sub"),
		tItem(1, "X Large Cheese Pizza", "X-Large", 1899, "p-pizza", tHalves()...),
	}
}

func manyItems(n int) []order.LineItem {
	var out []order.LineItem
	for i := 0; i < n; i++ {
		out = append(out, tItem(1+i%3, fmt.Sprintf("Garlic Knots Basket Number %d with Marinara", i+1), "", 499+int64(i), "p-fry"))
	}
	return out
}

func couponItems() []order.LineItem {
	a := tItem(2, "Large 1-Topping Pizza", "Large", 1250, "p-pizza", order.LineItem{Name: "Pepperoni", Quantity: 1})
	b := tItem(1, "Buffalo Wings 20 Piece Party Tray", "20 pc", 0, "p-fry", order.LineItem{Name: "Hot", Quantity: 1})
	c := tItem(1, "2 Liter Soda", "2 Liter", 0, "p-front", order.LineItem{Name: "Coca-Cola Zero Sugar", Quantity: 1})
	for _, it := range []*order.LineItem{&a, &b, &c} {
		it.Coupon, it.CouponNo = tCoupon, 1
	}
	return []order.LineItem{a, tItem(1, "Caesar Salad", "", 899, "p-front"), b, c}
}

func TestPrintTortureDump(t *testing.T) {
	dir := os.Getenv("PRINT_TORTURE_OUT")
	if dir == "" {
		t.Skip("PRINT_TORTURE_OUT not set (tests/printcheck runs this)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	paper := 576
	put := func(name string, data []byte, kind string, texts []string, totals ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name+".bin"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		e := tortureJob{Paper: paper, Source: "agent", Kind: kind, Texts: texts, Totals: totals}
		b, _ := json.MarshalIndent(e, "", " ")
		if err := os.WriteFile(filepath.Join(dir, name+".expect.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sc := tortureSC()
	def := order.DefaultReceiptPrefs()
	totals := []string{"Subtotal", "Tax", "Total"}

	receiptTexts := func(s order.Snapshot, sizes bool) []string {
		x := append(textsOf(s.Items, sizes), s.Notes)
		if a := s.Fulfillment.Address; a != nil && s.Fulfillment.Type == "delivery" {
			x = append(x, a.Line1, a.Line2, a.City)
		}
		for _, f := range s.Money.Fees {
			if f.Type != "delivery" || s.Fulfillment.Type == "delivery" {
				x = append(x, f.Label)
			}
		}
		for _, d := range s.Money.Discounts {
			x = append(x, d.Label)
		}
		return append(x, lastFirst(tCustomer), tStoreName)
	}
	receipt := func(name string, s order.Snapshot, prefs order.ReceiptPrefs, rerouted string) {
		data := Build(s, sc, printer.Ticket{Kind: printer.TicketReceipt, ReroutedFrom: rerouted}, prefs)
		x := receiptTexts(s, true)
		if prefs.Format == order.FormatCondensed {
			x = nil
			for _, it := range s.Items {
				x = append(x, it.Name)
			}
			x = append(x, s.Notes, lastFirst(tCustomer))
		}
		// a voided slip's items are struck-through pictures: the verifier reads them back (ocr.py)
		if rerouted != "" {
			x = append(x, rerouted)
		}
		put(name, data, "receipt", x, totals...)
	}

	items := tortureItems()
	for _, typ := range []string{"pickup", "delivery", "dine_in"} {
		receipt("receipt_"+typ+"_torture", tortureSnap(typ, items...), def, "")
	}
	receipt("receipt_delivery_many64", tortureSnap("delivery", manyItems(64)...), def, "")
	receipt("receipt_pickup_coupons", tortureSnap("pickup", couponItems()...), def, "")
	receipt("receipt_delivery_rerouted", tortureSnap("delivery", items[:2]...), def, tPrinter)
	cond := def
	cond.Format = order.FormatCondensed
	receipt("receipt_delivery_condensed", tortureSnap("delivery", items...), cond, "")
	v := tortureSnap("pickup", items...)
	v.Voided = "VOID"
	receipt("receipt_pickup_voided", v, def, "")
	tiny := tortureSnap("pickup", tItem(1, "Q", "", 100, "p-sub"))
	tiny.Customer = order.Customer{DisplayName: "Q"}
	tiny.Notes = "."
	tiny.Money = order.Money{Subtotal: 100, Total: 100}
	tiny.Payments = nil
	put("receipt_pickup_tiny", Build(tiny, sc, printer.Ticket{Kind: printer.TicketReceipt}, def), "receipt",
		[]string{"Q"}, "Subtotal", "Total")
	fut := tortureSnap("delivery", items[:3]...)
	due := fut.PlacedAt.Add(26 * time.Hour)
	fut.Fulfillment.PromisedAt = &due
	data := Build(fut, sc, printer.Ticket{Kind: printer.TicketReceipt, Future: true}, def)
	put("receipt_delivery_future", data, "receipt", receiptTexts(fut, true), totals...)

	// --- kitchen station tickets: full + "Also on this order", every change kind, reroute bands
	snap := tortureSnap("delivery", items...)
	mine, others := []order.LineItem{items[0], items[3], items[4], items[6]}, []order.LineItem{items[1], items[2], items[5]}
	stationTexts := func(own, rest []order.LineItem) []string {
		x := append(textsOf(own, false), snap.Notes, lastFirst(tCustomer))
		for _, it := range rest {
			x = append(x, it.Name)
		}
		return x
	}
	station := func(name string, s order.Snapshot, tk printer.Ticket, prefs order.ReceiptPrefs, texts []string) {
		tk.Kind, tk.TargetID, tk.Station = printer.TicketStation, "p-pizza", "Pizza"
		put(name, Build(s, sc, tk, prefs), "station", texts)
	}
	station("station_pizza_torture", snap, printer.Ticket{Mine: mine, Others: others}, def, stationTexts(mine, others))
	station("station_pizza_rerouted", snap, printer.Ticket{Mine: mine, Others: others, ReroutedFrom: tPrinter}, def,
		append(stationTexts(mine, others), tPrinter))
	// the same ticket landing on the store's 72 mm Online printer (512 dots, 42 columns): what a reroute prints there
	paper = 512
	station("station_pizza_rerouted_72mm", snap, printer.Ticket{Mine: mine, Others: others, ReroutedFrom: tPrinter}, def,
		append(stationTexts(mine, others), tPrinter))
	put("driver_report_72mm", BuildDriverReport(rep72(), sc, "", tPrinter, time.Date(2026, 11, 13, 3, 48, 0, 0, time.UTC)),
		"driver_report", []string{tStaff, tStreet, tPrinter})
	receipt("receipt_delivery_72mm", tortureSnap("delivery", items...), def, tPrinter)
	paper = 576
	for _, size := range []string{order.SizeNormal, order.SizeXLarge} {
		p := def
		p.KitchenItemSize = size
		station("station_pizza_items_"+size, snap, printer.Ticket{Mine: mine, Others: others}, p, stationTexts(mine, others))
	}
	station("station_sub_many64", tortureSnap("pickup", manyItems(64)...), printer.Ticket{Mine: manyItems(64)}, def,
		stationTexts(manyItems(64), nil))
	station("station_change_add_remove", snap, printer.Ticket{Change: true, Mine: []order.LineItem{items[0]}, Removed: []order.LineItem{items[3], items[4]}}, def,
		append(textsOf(items[:1], false), append(textsOf([]order.LineItem{items[3], items[4]}, false), "ADD / AGREGAR", "REMOVE / QUITAR")...))
	station("station_void", snap, printer.Ticket{Void: true, Mine: mine}, def, append(textsOf(mine, false), "REMOVE / QUITAR"))
	nostrike := def
	nostrike.StrikeImages = false
	station("station_void_nostrike", snap, printer.Ticket{Void: true, Mine: mine}, nostrike, textsOf(mine, false))
	moved := snap
	station("station_type_change", moved, printer.Ticket{Change: true, TypeOnly: true, WasType: "pickup", Mine: mine}, def,
		append(textsOf(mine, false), "WAS PICKUP"))
	fs := snap
	fs.Fulfillment.PromisedAt = &due
	station("station_future", fs, printer.Ticket{Mine: mine, Others: others, Future: true}, def, stationTexts(mine, others))
	tinyS := tiny
	station("station_tiny", tinyS, printer.Ticket{Mine: tinyS.Items}, def, []string{"Q"})

	// --- the driver report, the Unsettled Check List, the test slip
	k1, k2 := int64(1104), int64(1187)
	opened := time.Date(2026, 11, 12, 14, 2, 0, 0, time.UTC)
	closed := time.Date(2026, 11, 13, 3, 48, 0, 0, time.UTC)
	rep := order.DriverReport{BatchID: 3, BatchOpenedAt: opened, BatchClosedAt: &closed, DriverName: tStaff,
		Lines: []order.DriverReportLine{
			{KitchenNumber: &k1, Customer: tCustomer, Address: tStreet + ", " + tApt, Street: tStreet, Cash: true, CashCents: 999999, OrderTipCents: 2000, SlipTotalCents: 999999},
			{KitchenNumber: &k2, Source: "web", Customer: "Q", Street: "1 A St", TipCents: 500, OrderTipCents: 500, SlipTotalCents: 2900, TipPending: true},
		}}
	for i := 0; i < 30; i++ {
		k := int64(1200 + i)
		rep.Lines = append(rep.Lines, order.DriverReportLine{KitchenNumber: &k, Customer: "Ana P.", Street: fmt.Sprintf("%d %s", 100+i, tStreet), Cash: i%2 == 0, CashCents: 1850, SlipTotalCents: 1850})
	}
	rep.Finish(600)
	for _, size := range []string{"", order.SizeSmall, order.SizeLarge} {
		put("driver_report_"+map[string]string{"": "default", order.SizeSmall: "small", order.SizeLarge: "large"}[size],
			BuildDriverReport(rep, sc, size, tPrinter, closed), "driver_report", []string{tStaff, tStreet, tPrinter, tStoreName})
	}
	placed := time.Date(2026, 11, 12, 23, 15, 0, 0, time.UTC)
	ul := order.UnsettledList{BatchID: 41, Count: 2, DueCents: 1009998, Checks: []order.UnsettledCheck{
		{OrderID: "a", Number: 12, Type: "counter", State: order.StateReady, PlacedAt: placed, BatchID: 41, Carried: true,
			StaffName: tStaff, TotalCents: 999999, PaidCents: 1000, DueCents: 998999,
			Items: []order.UnsettledLineItem{{Qty: 99, Name: tName120, Mods: []string{tModLong, "Pepperoni (1st half)", "Jalapeños"}, PriceCents: 999999}}},
		{OrderID: "b", Number: 1107, Type: "delivery", Source: "web", State: order.StateReady, PlacedAt: placed, BatchID: 41,
			Customer: order.UnsettledCustomer{Name: tCustomer, Phone: "4105550100"}, TotalCents: 10999, DueCents: 10999,
			Items: []order.UnsettledLineItem{{Qty: 1, Name: tUnicode1, Mods: []string{}, PriceCents: 10999}}},
	}}
	put("unsettled_list_torture", BuildUnsettledList(ul, sc, "", tPrinter, closed), "unsettled",
		[]string{tName120, tModLong, tCustomer, tStaff, tUnicode1, tPrinter, tStoreName})
	put("test_slip_long_names", BuildTestSlip(tPrinter, sc, "Front Counter Tablet by the Register (Left)", closed), "test_slip",
		[]string{tPrinter, "Front Counter Tablet by the Register (Left)"})
	put("sample_receipt", BuildSample(sc, def, closed), "sample", nil, totals...)
}

// rep72 is a short driver report for the 72 mm printer variant.
func rep72() order.DriverReport {
	k := int64(1104)
	opened := time.Date(2026, 11, 12, 14, 2, 0, 0, time.UTC)
	rep := order.DriverReport{BatchID: 3, BatchOpenedAt: opened, DriverName: tStaff, Lines: []order.DriverReportLine{
		{KitchenNumber: &k, Customer: tCustomer, Street: tStreet, Cash: true, CashCents: 999999, OrderTipCents: 2000, SlipTotalCents: 999999}}}
	rep.Finish(600)
	return rep
}
