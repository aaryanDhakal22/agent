package receipt

import (
	"reflect"
	"strings"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

// Text from outside never reaches a printer as it came (the security review, 2026-11-12, H3): a customer
// name, a note, an address, an item or modifier name is whatever a vendor, a website or a terminal sent.
// With ESC p in it the cash drawer opened, with GS V the paper was cut, with DLE DC4 the printer answered
// in real time, and a note "ring bell\nPAID - CARD" printed PAID - CARD as a line of its own.
//
// Every builder of this package starts with one scrub of everything it is handed (the order, the ticket,
// the store, the settings, a report): each string loses its control bytes (cleanText) and is cut to a
// length a slip can carry. After it the only control bytes in a job are the builder's own commands.
// printable() (halves.go), dclean (delivery_copies.go) and toPC437 (pc437.go) clean once more what they
// write, so a text built inside a builder is covered too.

// The longest a text prints (characters). main cuts what it stores to the same lengths.
const (
	// MaxNameRunes: a person or a thing by name (the customer, a driver, staff, a station / printer, the
	// store, a tablet) and a phone number.
	MaxNameRunes = 80
	// MaxAddressRunes: one line of an address (street, apartment, city, state, zip, country).
	MaxAddressRunes = 120
	// MaxNoteRunes: a note (the order's, an item's, the delivery directions, the footer) and every other
	// text on a slip (an item, size, modifier or coupon name, a fee or discount label).
	MaxNoteRunes = 300
)

// cleanText is the one sanitizer: s without its control bytes. A line end or a tab becomes a space (CR LF
// one space), every other byte below 0x20 and 0x7F goes. Bytes from 0x80 stay (UTF-8, or a byte already in
// the printer's code page); toPC437 drops the control characters among those.
func cleanText(s string) string {
	i := 0
	for ; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			break
		}
	}
	if i == len(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(s[:i])
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\r' && i+1 < len(s) && s[i+1] == '\n': // the \n after it is the space
		case c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r':
			b.WriteByte(' ')
		case c < 0x20 || c == 0x7f:
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// clip cuts s to its first n characters.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// cleanLines is cleanText for the one text a builder itself splits into lines, the receipt footer: its
// line ends stay, each line is cleaned.
func cleanLines(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return clip(cleanText(s), MaxNoteRunes)
	}
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n"), "\n")
	for i, l := range lines {
		lines[i] = cleanText(l)
	}
	return clip(strings.Join(lines, "\n"), MaxNoteRunes)
}

func cleanName(s string) string    { return clip(cleanText(s), MaxNameRunes) }
func cleanAddress(s string) string { return clip(cleanText(s), MaxAddressRunes) }

// scrubbed is a copy of v (slices, maps and pointers copied, so the caller's order is never changed) with
// every string in it cleaned and cut to MaxNoteRunes. Byte slices and unexported fields (a time) stay as
// they are. It reaches every string by reflection, so a field added later is covered without a line here.
func scrubbed[T any](v T) T {
	return mapStrings(v, func(s string) string { return clip(cleanText(s), MaxNoteRunes) })
}

// mapStrings is a deep copy of v with f applied to every string in it (map keys aside).
func mapStrings[T any](v T, f func(string) string) T {
	out := reflect.New(reflect.TypeOf(&v).Elem()).Elem()
	out.Set(reflect.ValueOf(&v).Elem())
	mapValue(out, f)
	return out.Interface().(T)
}

func mapValue(v reflect.Value, f func(string) string) {
	switch v.Kind() {
	case reflect.String:
		if s, c := v.String(), f(v.String()); c != s {
			v.SetString(c)
		}
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		c := reflect.New(v.Type().Elem())
		c.Elem().Set(v.Elem())
		mapValue(c.Elem(), f)
		v.Set(c)
	case reflect.Slice:
		if v.IsNil() || v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		reflect.Copy(c, v)
		for i := 0; i < c.Len(); i++ {
			mapValue(c.Index(i), f)
		}
		v.Set(c)
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			mapValue(v.Index(i), f)
		}
	case reflect.Map:
		if v.IsNil() {
			return
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			val := reflect.New(v.Type().Elem()).Elem()
			val.Set(it.Value())
			mapValue(val, f)
			c.SetMapIndex(it.Key(), val)
		}
		v.Set(c)
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		c := reflect.New(v.Elem().Type()).Elem()
		c.Set(v.Elem())
		mapValue(c, f)
		v.Set(c)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if fld := v.Field(i); fld.CanSet() {
				mapValue(fld, f)
			}
		}
	}
}

// scrubSnapshot is the order as it may print: every text cleaned, names and address lines cut shorter.
func scrubSnapshot(s order.Snapshot) order.Snapshot {
	s = scrubbed(s)
	s.Customer.DisplayName = cleanName(s.Customer.DisplayName)
	s.Customer.Phone = cleanName(s.Customer.Phone)
	s.DriverName = cleanName(s.DriverName)
	s.TakenBy = cleanName(s.TakenBy)
	if a := s.Fulfillment.Address; a != nil { // scrubbed's own copy
		a.Line1, a.Line2, a.City = cleanAddress(a.Line1), cleanAddress(a.Line2), cleanAddress(a.City)
		a.State, a.PostalCode, a.Country = cleanAddress(a.State), cleanAddress(a.PostalCode), cleanAddress(a.Country)
	}
	return s
}

// scrubStore is the store's identity as it may print (its name, address, phone, the footer and website
// in its config).
func scrubStore(sc storeconfig.StoreContext) storeconfig.StoreContext {
	footer, hasFooter := sc.Location.Config[footerKey].(string)
	sc = scrubbed(sc)
	if hasFooter { // the footer keeps its lines (cleanLines); Config is scrubbed's own copy
		sc.Location.Config[footerKey] = cleanLines(footer)
	}
	sc.Tenant.Name = cleanName(sc.Tenant.Name)
	sc.Location.Name = cleanName(sc.Location.Name)
	sc.Location.Phone = cleanName(sc.Location.Phone)
	sc.Location.AddressLine1 = cleanAddress(sc.Location.AddressLine1)
	sc.Location.AddressLine2 = cleanAddress(sc.Location.AddressLine2)
	for i := range sc.Printers {
		sc.Printers[i].Name = cleanName(sc.Printers[i].Name)
	}
	return sc
}

// footerKey is the location config's receipt footer (LocationConfigString("receipt_footer", "")).
const footerKey = "receipt_footer"

// scrubPrefs is the receipt settings as they may print: the footer text cleaned, its lines kept.
func scrubPrefs(p order.ReceiptPrefs) order.ReceiptPrefs {
	footer := p.FooterText
	p = scrubbed(p)
	if footer != nil {
		f := cleanLines(*footer)
		p.FooterText = &f
	}
	return p
}

// scrubTicket is the ticket as it may print: its items, the station and the printer it was rerouted from.
func scrubTicket(t printer.Ticket) printer.Ticket {
	t = scrubbed(t)
	t.Station = cleanName(t.Station)
	t.ReroutedFrom = cleanName(t.ReroutedFrom)
	return t
}

func scrubPaper(p printer.Paper) printer.Paper {
	p.ReroutedFrom = cleanName(p.ReroutedFrom)
	return p
}

func scrubDriverLine(l *order.DriverReportLine) {
	l.Customer = cleanName(l.Customer)
	l.Address, l.Street = cleanAddress(l.Address), cleanAddress(l.Street)
}

func scrubDriverReport(r order.DriverReport) order.DriverReport {
	r = scrubbed(r)
	r.DriverName = cleanName(r.DriverName)
	for i := range r.Lines {
		scrubDriverLine(&r.Lines[i])
	}
	return r
}

func scrubUnassignedReport(r order.UnassignedReport) order.UnassignedReport {
	r = scrubbed(r)
	for i := range r.Lines {
		l := &r.Lines[i]
		scrubDriverLine(&l.DriverReportLine)
		l.MarkedBy.Name = cleanName(l.MarkedBy.Name)
		l.FromDriver = cleanName(l.FromDriver)
		for j := range l.Available {
			l.Available[j] = cleanName(l.Available[j])
		}
	}
	return r
}

func scrubUnsettledList(l order.UnsettledList) order.UnsettledList {
	l = scrubbed(l)
	for i := range l.Checks {
		c := &l.Checks[i]
		c.Customer.Name, c.Customer.Phone = cleanName(c.Customer.Name), cleanName(c.Customer.Phone)
		c.StaffName = cleanName(c.StaffName)
	}
	return l
}
