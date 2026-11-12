package receipt

// Text from outside can't drive the printer (the security review, H3): control bytes in any text of an
// order, a ticket, the store, the settings or a report print nothing, a command typed into a name stays
// text, a line end in a note can't forge a line, and long texts are cut. Every slip kind, every width.

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

// ctlGarbage is control bytes that must vanish without a trace (none of the ones that become a space):
// ESC, GS, DLE, DC4, FS, NUL, BEL, BS, DEL, US (the builders' own glue byte), CAN, ENQ.
const ctlGarbage = "\x1b\x1d\x10\x14\x1c\x00\x07\x08\x7f\x1f\x18\x05"

// The commands a customer could type into a name: drawer kick, real-time drawer pulse, cut (two forms),
// reset, print the stored logo, select a code page, a real-time status request.
var typedCommands = []string{"\x1bp\x00\x19\xfa", "\x10\x14\x01\x00\x05", "\x1dV\x00", "\x1dVA\x03", "\x1b@", "\x1cp\x01\x00", "\x1bt\x10", "\x10\x04\x01"}

// poison puts ctlGarbage at the start, in the middle (on a character boundary) and at the end of s.
func poison(s string) string {
	mid := len(s) / 2
	for mid > 0 && !utf8.RuneStart(s[mid]) {
		mid--
	}
	return ctlGarbage + s[:mid] + ctlGarbage + s[mid:] + ctlGarbage
}

// attack is s with every typed command in it: before, inside and after.
func attack(s string) string {
	all := strings.Join(typedCommands, "")
	mid := len(s) / 2
	for mid > 0 && !utf8.RuneStart(s[mid]) {
		mid--
	}
	return typedCommands[0] + s[:mid] + all + s[mid:] + typedCommands[1] + typedCommands[2]
}

var (
	refLineEnds = regexp.MustCompile(`\r\n|[\t\n\v\f\r]`)
	refControls = regexp.MustCompile(`[\x00-\x1f\x7f]`)
)

// refClean is what cleanText must do, written another way.
func refClean(s string) string {
	return refControls.ReplaceAllString(refLineEnds.ReplaceAllString(s, " "), "")
}

// slips builds every slip kind for a printer of cols columns from the torture corpus, each input passed
// through f first (the identity = the clean slip).
func slips(t *testing.T, cols int, f func(string) string) map[string][]byte {
	t.Helper()
	old := deliveryNow
	deliveryNow = func() time.Time { return time.Date(2026, 11, 12, 22, 41, 0, 0, time.UTC) }
	defer func() { deliveryNow = old }()

	out := map[string][]byte{}
	footer := "Thank You\nPlease Come Again"
	sc := tortureSC()
	sc.Location.Phone = "4103219288"
	sc.Location.Config = map[string]any{"website": "lombardis.example", "receipt_footer": footer}
	sc.Tenant.Name = "Lombardi's"
	sc = mapStrings(sc, f)
	def := order.DefaultReceiptPrefs()
	typed := "See you soon"
	withFooter := def
	withFooter.FooterText = &typed
	withFooter = mapStrings(withFooter, f)
	cond := def
	cond.Format = order.FormatCondensed
	nostrike := def
	nostrike.StrikeImages = false
	rerouted := f(tPrinter)
	at := time.Date(2026, 11, 13, 3, 48, 0, 0, time.UTC)

	items := tortureItems()
	receipt := func(name string, s order.Snapshot, prefs order.ReceiptPrefs, from string, future bool) {
		out[name] = Build(mapStrings(s, f), sc, printer.Ticket{Kind: printer.TicketReceipt, ReroutedFrom: from, Future: future, Cols: cols}, prefs)
	}
	for _, typ := range []string{"pickup", "delivery", "dine_in"} {
		receipt("receipt_"+typ, tortureSnap(typ, items...), def, "", false)
	}
	cash := tortureSnap("delivery", items...)
	cash.Payments = nil
	cash.TakenBy = "Call center - " + tStaff
	cash.DriverName = tStaff
	receipt("receipt_delivery_cash", cash, withFooter, "", false)
	receipt("receipt_coupons", tortureSnap("pickup", couponItems()...), def, "", false)
	receipt("receipt_rerouted", tortureSnap("delivery", items...), def, rerouted, false)
	receipt("receipt_condensed", tortureSnap("delivery", items...), cond, "", false)
	voided := tortureSnap("pickup", items...)
	voided.Voided = "VOID"
	receipt("receipt_voided", voided, def, "", false)
	receipt("receipt_voided_nostrike", voided, nostrike, "", false)
	fut := tortureSnap("delivery", items[:3]...)
	due := fut.PlacedAt.Add(26 * time.Hour)
	fut.Fulfillment.PromisedAt = &due
	receipt("receipt_future", fut, def, "", true)
	vendor := tortureSnap("pickup", items[:1]...)
	vendor.KitchenNumber = nil
	vendor.SourceEventID = "WEB-2026-11-12-000123456789"
	receipt("receipt_vendor_number", vendor, def, "", false)
	out["receipt_buildwith"] = BuildWith(mapStrings(cash, f), sc)

	dc := mapStrings(cash, f)
	out["delivery_copies"] = Build(dc, sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Cols: cols}, withFooter)
	out["delivery_copies_rerouted"] = BuildDeliveryCopies(dc, sc, def, printer.Paper{Cols: cols, ReroutedFrom: rerouted})

	mine, others := []order.LineItem{items[0], items[3], items[4], items[6]}, []order.LineItem{items[1], items[2], items[5]}
	station := func(name string, s order.Snapshot, tk printer.Ticket, prefs order.ReceiptPrefs) {
		tk.Kind, tk.TargetID, tk.Station, tk.Cols = printer.TicketStation, "p-pizza", "Pizza", cols
		out[name] = Build(mapStrings(s, f), sc, mapStrings(tk, f), prefs)
	}
	station("station", cash, printer.Ticket{Mine: mine, Others: others}, def)
	station("station_rerouted", cash, printer.Ticket{Mine: mine, Others: others, ReroutedFrom: tPrinter}, def)
	station("station_change", cash, printer.Ticket{Change: true, Mine: items[:1], Removed: []order.LineItem{items[3], items[4]}}, def)
	station("station_void", cash, printer.Ticket{Void: true, Mine: mine}, def)
	station("station_void_nostrike", cash, printer.Ticket{Void: true, Mine: mine}, nostrike)
	station("station_type_change", cash, printer.Ticket{Change: true, TypeOnly: true, WasType: "pickup", Mine: mine}, def)
	station("station_future", fut, printer.Ticket{Mine: mine, Others: others, Future: true}, def)
	station("station_vendor_number", vendor, printer.Ticket{Mine: vendor.Items}, def)
	tk := mapStrings(printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza", Mine: mine, Others: others, Cols: cols}, f)
	out["station_buildstation"] = BuildStation(mapStrings(cash, f), tk)

	on := printer.Paper{Cols: cols, ReroutedFrom: rerouted}
	k1, k2 := int64(1104), int64(1187)
	opened := time.Date(2026, 11, 12, 14, 2, 0, 0, time.UTC)
	rep := order.DriverReport{BatchID: 3, BatchOpenedAt: opened, BatchClosedAt: &at, DriverName: tStaff, DriverID: "d-1",
		Lines: []order.DriverReportLine{
			{KitchenNumber: &k1, Customer: tCustomer, Address: tStreet + ", " + tApt, Street: tStreet, Cash: true, CashCents: 999999, OrderTipCents: 2000, SlipTotalCents: 999999},
			{KitchenNumber: &k2, Source: "web", Customer: "Q", Street: "1 A St", TipCents: 500, OrderTipCents: 500, SlipTotalCents: 2900, TipPending: true},
			{OrderID: "o-3", Source: "counter", Customer: "Ana P.", Street: "7 " + tStreet, SlipTotalCents: 1850},
		}}
	rep.Finish(600)
	out["driver_report"] = BuildDriverReport(mapStrings(rep, f), sc, "", on, at)
	out["driver_report_small"] = BuildDriverReport(mapStrings(rep, f), sc, order.SizeSmall, printer.Paper{Cols: cols}, at)
	un := order.UnassignedReport{BatchID: 3, BatchOpenedAt: opened, BatchClosedAt: &at, CashCents: 999999, TipCents: 2000,
		Lines: []order.UnassignedLine{{DriverReportLine: order.DriverReportLine{KitchenNumber: &k1, Street: tStreet, Address: tStreet + ", " + tApt,
			Cash: true, SlipTotalCents: 999999, OrderTipCents: 2000}, MarkedAt: at, MarkedBy: order.MarkedBy{Kind: "tablet", Name: tStaff},
			FromDriver: tStaff, Available: []string{tStaff, "Ana P."}}}}
	out["unassigned_report"] = BuildUnassignedReport(mapStrings(un, f), sc, "", on, at)
	placed := time.Date(2026, 11, 12, 23, 15, 0, 0, time.UTC)
	ul := order.UnsettledList{BatchID: 41, Count: 2, DueCents: 1009998, Checks: []order.UnsettledCheck{
		{OrderID: "a", Number: 12, Type: "counter", State: order.StateReady, PlacedAt: placed, BatchID: 41, Carried: true,
			StaffName: tStaff, TotalCents: 999999, PaidCents: 1000, DueCents: 998999,
			Items: []order.UnsettledLineItem{{Qty: 99, Name: tName120, Mods: []string{tModLong, "Pepperoni (1st half)"}, PriceCents: 999999}}},
		{OrderID: "b", Number: 1107, Type: "delivery", Source: "web", State: order.StateReady, PlacedAt: placed, BatchID: 41,
			Customer: order.UnsettledCustomer{Name: tCustomer, Phone: "4105550100"}, TotalCents: 10999, DueCents: 10999,
			Items: []order.UnsettledLineItem{{Qty: 1, Name: tUnicode1, Mods: []string{}, PriceCents: 10999}}},
	}}
	out["unsettled_list"] = BuildUnsettledList(mapStrings(ul, f), sc, "", on, at)
	out["test_slip"] = BuildTestSlip(f(tPrinter), sc, f("Front Counter Tablet"), at, cols)
	out["width_test"] = BuildWidthTest(f(tPrinter), at)
	out["sample"] = BuildSample(sc, withFooter, at, cols)
	out["future_sample"] = BuildFutureSample(sc, withFooter, at, true, cols)
	return out
}

// jobAudit is a job read the way a printer reads it: the commands by name, the text lines, and what a
// printer would not take as text or as one of the builders' commands.
type jobAudit struct {
	cmds  map[string]int
	lines []string
	bad   []string
}

func auditJob(b []byte) jobAudit {
	a := jobAudit{cmds: map[string]int{}}
	var line []byte
	arg := func(i, n int) int { // i + n when the job has that many bytes left
		if i+n > len(b) {
			a.bad = append(a.bad, fmt.Sprintf("a command cut short at %d", i))
			return len(b)
		}
		return i + n
	}
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == '\n':
			a.lines = append(a.lines, string(line))
			line = line[:0]
			i++
		case c == 0x1b && i+1 < len(b):
			switch op := b[i+1]; op {
			case '@':
				a.cmds["ESC @"]++
				i += 2
			case 'a', 'E', '!', 'J':
				a.cmds["ESC "+string(op)]++
				i = arg(i, 3)
			case '*':
				a.cmds["ESC *"]++
				if i+5 > len(b) {
					a.bad = append(a.bad, "ESC * cut short")
					i = len(b)
					break
				}
				per := 1
				if b[i+2] == 32 || b[i+2] == 33 {
					per = 3
				}
				i = arg(i, 5+per*(int(b[i+3])+256*int(b[i+4])))
			default:
				a.bad = append(a.bad, fmt.Sprintf("ESC 0x%02x at %d", op, i))
				i += 2
			}
		case c == 0x1d && i+1 < len(b):
			switch op := b[i+1]; op {
			case '!', 'B':
				a.cmds["GS "+string(op)]++
				i = arg(i, 3)
			case 'V':
				a.cmds["GS V"]++
				if i+2 < len(b) && (b[i+2] == 65 || b[i+2] == 66) {
					i = arg(i, 4)
				} else {
					i = arg(i, 3)
				}
			case 'v':
				a.cmds["GS v"]++
				if i+8 > len(b) || b[i+2] != '0' {
					a.bad = append(a.bad, "GS v cut short")
					i = len(b)
					break
				}
				i = arg(i, 8+(int(b[i+4])+256*int(b[i+5]))*(int(b[i+6])+256*int(b[i+7])))
			default:
				a.bad = append(a.bad, fmt.Sprintf("GS 0x%02x at %d", op, i))
				i += 2
			}
		case c < 0x20 || c == 0x7f:
			a.bad = append(a.bad, fmt.Sprintf("control byte 0x%02x at %d", c, i))
			i++
		default:
			line = append(line, c)
			i++
		}
	}
	return a
}

var allWidths = []int{Cols80, Cols72, Cols58}

// Control bytes in every string of every input print nothing: the job is the clean job, byte for byte.
// mapStrings reaches every string by reflection, so a field added to the order later is poisoned too.
func TestScrubControlBytesPrintNothing(t *testing.T) {
	for _, cols := range allWidths {
		clean, dirty := slips(t, cols, func(s string) string { return s }), slips(t, cols, poison)
		if len(clean) < 30 {
			t.Fatalf("only %d slips built", len(clean))
		}
		for name, want := range clean {
			if got := dirty[name]; !bytes.Equal(got, want) {
				t.Errorf("%s at %d columns: control bytes in its texts changed the job (%d bytes, clean %d)%s", name, cols, len(got), len(want), firstDiff(got, want))
			}
			if a := auditJob(want); len(a.bad) > 0 {
				t.Errorf("%s at %d columns: the clean job itself has %v", name, cols, a.bad)
			}
		}
	}
}

func firstDiff(got, want []byte) string {
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			lo, hi := max(i-30, 0), min(i+30, len(got), len(want))
			return fmt.Sprintf("\n first difference at %d:\n  got  %q\n  want %q", i, got[lo:hi], want[lo:hi])
		}
	}
	return ""
}

// ESC p, DLE DC4, GS V, ESC @ ... typed into every text: no slip carries a drawer kick or a real-time
// command, none has a cut or a reset more than its clean twin, and nothing in it is a command the
// builders don't use. What is left of the typed command is its printable letters, as text.
func TestScrubTypedCommandsStayText(t *testing.T) {
	for _, cols := range allWidths {
		// The footer is the one text with lines of its own (the corpus has no other line end): they stay.
		twin := func(s string) string {
			lines := strings.Split(attack(s), "\n")
			for i := range lines {
				lines[i] = refClean(lines[i])
			}
			return strings.Join(lines, "\n")
		}
		dirty, clean := slips(t, cols, attack), slips(t, cols, twin)
		for name, job := range dirty {
			a, want := auditJob(job), auditJob(clean[name])
			if len(a.bad) > 0 {
				t.Errorf("%s at %d columns: %v", name, cols, a.bad[:min(len(a.bad), 5)])
			}
			for _, cmd := range []string{"GS V", "ESC @", "ESC *", "GS v", "GS B", "ESC J"} {
				if a.cmds[cmd] != want.cmds[cmd] {
					t.Errorf("%s at %d columns: %d x %s, the clean slip has %d", name, cols, a.cmds[cmd], cmd, want.cmds[cmd])
				}
			}
			for _, raw := range [][]byte{[]byte("\x1bp"), {0x10, 0x14}, {0x10, 0x04}, []byte("\x1cp"), []byte("\x1bt"), []byte("\x1dV\x00"), []byte("\x1dVA")} {
				if stripPictures(job, raw) {
					t.Errorf("%s at %d columns: the job carries % x", name, cols, raw)
				}
			}
			if !bytes.Equal(job, clean[name]) {
				t.Errorf("%s at %d columns: not the job of the same texts without their control bytes%s", name, cols, firstDiff(job, clean[name]))
			}
		}
	}
}

// stripPictures reports whether raw is in the job outside its pictures (a picture's dots are any bytes).
func stripPictures(job, raw []byte) bool {
	var text []byte
	for i := 0; i < len(job); {
		switch {
		case job[i] == 0x1b && i+4 < len(job) && job[i+1] == '*':
			per := 1
			if job[i+2] == 32 || job[i+2] == 33 {
				per = 3
			}
			i += 5 + per*(int(job[i+3])+256*int(job[i+4]))
			text = append(text, 0xff) // a picture splits what is around it
		case job[i] == 0x1d && i+7 < len(job) && job[i+1] == 'v' && job[i+2] == '0':
			i += 8 + (int(job[i+4])+256*int(job[i+5]))*(int(job[i+6])+256*int(job[i+7]))
			text = append(text, 0xff)
		default:
			text = append(text, job[i])
			i++
		}
	}
	return bytes.Contains(text, raw)
}

// A line end in a note can't forge a line of the slip: "PAID - CARD" under a cash order's note stays
// inside the note.
func TestScrubNoteCannotForgeALine(t *testing.T) {
	sc := tortureSC()
	for _, note := range []string{"ring bell\nPAID - CARD", "ring bell\r\nPAID - CARD", "ring bell\rPAID - CARD", "ring bell\u2028PAID - CARD", "ring bell\vPAID - CARD"} {
		snap := tortureSnap("delivery", tItem(1, "Cheese Pizza", "Large", 1200, "p-pizza", order.LineItem{Name: "Pepperoni\nPAID - CARD", Quantity: 1}))
		snap.Payments = nil // cash: the builder itself never says PAID - CARD
		snap.Notes = note
		snap.Items[0].Notes = note
		snap.Customer.DisplayName = "Ann\nPAID - CARD"
		snap.Fulfillment.Address.Line2 = "4B\nPAID - CARD"
		snap.Fulfillment.Address.Notes = note
		for _, cols := range allWidths {
			jobs := map[string][]byte{
				"receipt":   Build(snap, sc, printer.Ticket{Kind: printer.TicketReceipt, Cols: cols}, order.DefaultReceiptPrefs()),
				"condensed": Build(snap, sc, printer.Ticket{Kind: printer.TicketReceipt, Cols: cols}, order.ReceiptPrefs{Format: order.FormatCondensed}),
				"station": Build(snap, sc, printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza",
					Mine: snap.Items, Cols: cols}, order.DefaultReceiptPrefs()),
				"copies": Build(snap, sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Cols: cols}, order.DefaultReceiptPrefs()),
			}
			for name, job := range jobs {
				a := auditJob(job)
				if len(a.bad) > 0 {
					t.Errorf("%s %q at %d: %v", name, note, cols, a.bad)
				}
				for _, l := range a.lines {
					if s := strings.TrimSpace(l); strings.HasPrefix(s, "PAID") {
						t.Errorf("%s at %d columns, note %q: the slip has a line %q the store never printed", name, cols, note, s)
					}
				}
				if !bytes.Contains(job, []byte("PAID - CARD")) && name != "condensed" {
					t.Errorf("%s at %d columns: the note's words are gone", name, cols)
				}
			}
		}
	}
}

// Long texts are cut: a name at MaxNameRunes, an address line at MaxAddressRunes, a note (and an item
// name) at MaxNoteRunes characters.
func TestScrubCaps(t *testing.T) {
	long := func(mark string, n int) string { return strings.Repeat(mark, n) }
	snap := tortureSnap("delivery", tItem(1, long("i", 1000), long("z", 1000), 1200, "p-pizza", order.LineItem{Name: long("m", 1000), Quantity: 1}))
	snap.Customer.DisplayName = long("ñ", 1000)
	snap.Notes = long("n", 1000)
	snap.Items[0].Notes = long("o", 1000)
	snap.Fulfillment.Address.Line1 = long("a", 1000)
	snap.Fulfillment.Address.Notes = long("d", 1000)
	snap.TakenBy = long("t", 1000)
	got := scrubSnapshot(snap)
	for what, c := range map[string]struct {
		s    string
		want int
	}{
		"customer name": {got.Customer.DisplayName, MaxNameRunes}, "taken by": {got.TakenBy, MaxNameRunes},
		"address line": {got.Fulfillment.Address.Line1, MaxAddressRunes},
		"order note":   {got.Notes, MaxNoteRunes}, "item note": {got.Items[0].Notes, MaxNoteRunes},
		"directions": {got.Fulfillment.Address.Notes, MaxNoteRunes}, "item name": {got.Items[0].Name, MaxNoteRunes},
		"size": {got.Items[0].Size, MaxNoteRunes}, "modifier": {got.Items[0].Children[0].Name, MaxNoteRunes},
	} {
		if n := utf8.RuneCountInString(c.s); n != c.want {
			t.Errorf("%s: %d characters, want %d", what, n, c.want)
		}
	}
	if !utf8.ValidString(got.Customer.DisplayName) {
		t.Error("a name cut in the middle of a character")
	}
	// The caller's order is not touched (the scrub works on a copy).
	if len(snap.Notes) != 1000 || len(snap.Items[0].Name) != 1000 || len(snap.Items[0].Children[0].Name) != 1000 || len(snap.Fulfillment.Address.Line1) != 1000 {
		t.Error("the scrub changed the caller's order")
	}
	job := Build(snap, tortureSC(), printer.Ticket{Kind: printer.TicketReceipt, Cols: Cols80}, order.DefaultReceiptPrefs())
	if n := bytes.Count(job, []byte("n")); n > MaxNoteRunes+200 {
		t.Errorf("the receipt prints %d of the note's 1000 characters", n)
	}
	rep := scrubDriverReport(order.DriverReport{DriverName: long("x", 500), Lines: []order.DriverReportLine{{Customer: long("c", 500), Street: long("s", 500), Address: long("a", 500)}}})
	if len(rep.DriverName) != MaxNameRunes || len(rep.Lines[0].Customer) != MaxNameRunes || len(rep.Lines[0].Street) != MaxAddressRunes || len(rep.Lines[0].Address) != MaxAddressRunes {
		t.Errorf("driver report caps: %d %d %d %d", len(rep.DriverName), len(rep.Lines[0].Customer), len(rep.Lines[0].Street), len(rep.Lines[0].Address))
	}
	st := scrubStore(storeconfig.StoreContext{Location: storeconfig.LocationContext{Name: long("s", 500), AddressLine1: long("a", 500),
		Config: map[string]any{"receipt_footer": "Thank you\r\n\x1bpCome again\x07", "website": "a\x1bb"}}})
	if len(st.Location.Name) != MaxNameRunes || len(st.Location.AddressLine1) != MaxAddressRunes {
		t.Errorf("store caps: %d %d", len(st.Location.Name), len(st.Location.AddressLine1))
	}
	if f := st.LocationConfigString("receipt_footer", ""); f != "Thank you\npCome again" {
		t.Errorf("footer = %q: its lines stay, its control bytes go", f)
	}
	if w := st.LocationConfigString("website", ""); w != "ab" {
		t.Errorf("website = %q", w)
	}
}

func TestCleanText(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "",
		"Maria Lopez":                  "Maria Lopez",
		"Jalape\u00f1o \xab \xfa":      "Jalape\u00f1o \xab \xfa", // UTF-8 and bytes already in the code page stay
		"a\tb\nc\r\nd\re\vf\fg":        "a b c d e f g",
		"\x1bp\x00\x19\xfaBob":         "p\xfaBob",
		"Bob\x10\x14\x01\x00\x05":      "Bob",
		"\x1dV\x00\x1dVB\x03\x1b@Bob":  "VVB@Bob",
		"a\x00b\x07c\x08d\x1fe\x7ff":   "abcdef",
		"line one\nPAID - CARD":        "line one PAID - CARD",
		"\x1b\x1b\x1b":                 "",
		"x\x1b":                        "x",
		"only\r":                       "only ",
		"\U0001F355 pizza\x1b@ \u00bd": "\U0001F355 pizza@ \u00bd",
	} {
		if got := cleanText(in); got != want {
			t.Errorf("cleanText(%q) = %q, want %q", in, got, want)
		}
		if got := cleanText(cleanText(in)); got != want {
			t.Errorf("cleanText twice (%q) = %q, want %q", in, got, want)
		}
		if got := refClean(in); got != want {
			t.Errorf("refClean(%q) = %q, want %q (the tests' own cleaner)", in, got, want)
		}
	}
	if got := clip("ñandú", 3); got != "ñan" {
		t.Errorf("clip = %q", got)
	}
	if got := clip("abc", 3); got != "abc" {
		t.Errorf("clip = %q", got)
	}
}

// The writers clean what they are handed, whatever built it: printable() (a line of a ticket), dclean
// (the delivery copies), toPC437 (the control characters that arrive as characters).
func TestWritersDropControlBytes(t *testing.T) {
	if got := printable("1\x1f\x1fCHEESE \u00bd\x1fPEP\x1bp\x00\x19\x19 \x10\x14\x1dV\x00"); got != "1  CHEESE \xab PEPp V" {
		t.Errorf("printable = %q", got)
	}
	if got := dclean("Jalape\u00f1o\x1bp\x00 \x1dVA\tdip"); got != "Jalape\xa4op VA dip" {
		t.Errorf("dclean = %q", got)
	}
	if got := toPC437("a\u0085b\u009bc\u2028d\u2029e\u00f1"); got != "abc d e\xa4" {
		t.Errorf("toPC437 = %q", got)
	}
	// A command of a builder still passes the writer.
	if got := toPC437(cmdCut + cmdInit + "\u00f1"); got != cmdCut+cmdInit+"\xa4" {
		t.Errorf("toPC437 touched a command: %q", got)
	}
	for _, l := range RerouteLines("Pizza\x1bp\x00\x19\x19\nOven", Cols80) {
		if a := auditJob([]byte(l)); len(a.bad) > 0 {
			t.Errorf("RerouteLines: %q has %v", l, a.bad)
		}
	}
}
