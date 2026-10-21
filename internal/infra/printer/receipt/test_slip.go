package receipt

import (
	"bytes"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/storeconfig"
)

// BuildTestSlip renders the short slip the tablet's "test print" puts on one
// printer: which printer it was meant for (so a slip on the wrong printer is
// obvious), the store, the tablet that asked and the local time. ASCII only —
// names typed with other characters are shown with '?' rather than risking
// codepage garbage on the very slip that is supposed to prove the printer works.
func BuildTestSlip(printerName string, sc storeconfig.StoreContext, tablet string, at time.Time) []byte {
	loc := time.Local
	if tz, err := time.LoadLocation(sc.Location.Timezone); sc.Location.Timezone != "" && err == nil {
		loc = tz
	}
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(s) }
	nl := func() { buf.WriteString(cmdFeed) }

	w(cmdInit)
	w(cmdBaseSz)
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w("TEST - Quibase setup")
	nl()
	nl()
	w(cmdQuadSz)
	w(truncRunes(strings.ToUpper(asciiOnly(printerName)), 12))
	nl()
	w(cmdBaseSz)
	w(cmdBoldOff)
	nl()
	w(cmdLeft)
	w(separator())
	nl()
	if store := asciiOnly(sc.StoreName()); store != "" {
		w(truncRunes("Store:   "+store, lineWidth))
		nl()
	}
	if t := asciiOnly(tablet); t != "" {
		w(truncRunes("Tablet:  "+t, lineWidth))
		nl()
	}
	w("Printed: " + at.In(loc).Format("Mon Jan 2 3:04 PM"))
	nl()
	w(separator())
	nl()
	w(cmdCenter)
	w(cmdBoldOn)
	w("If you can read this, this printer works.")
	w(cmdBoldOff)
	nl()

	w(cmdFeed + cmdFeed + cmdFeed + cmdFeed)
	w(cmdCut)
	return buf.Bytes()
}

// asciiOnly replaces every non-printable-ASCII character with '?' and trims.
func asciiOnly(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r >= 0x20 && r < 0x7F {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}
