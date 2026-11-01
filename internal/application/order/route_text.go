package order

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
)

// The route message (2.4.0, owner): one line per stop in run order, the
// ticket number and street + apt only —
//
//	#323 4108 Old Court Rd Apt 2B
//	#224 12 Main St
//
// — and the map link. (2.3.10 also listed CASH / PAID, the notes and the
// drinks and desserts to grab; that list wasn't reliable for every store,
// so the receipts on the bags carry it.)

// routeMsgMax is Pushover's message limit (characters).
const routeMsgMax = 1024

// stopHead is "#323 4108 Old Court Rd Apt 2B".
func stopHead(so order.StoredOrder) string {
	h := label(so)
	if a := so.Order.Fulfillment.Address; a != nil {
		street := strings.TrimSpace(a.Line1)
		if l2 := strings.TrimSpace(a.Line2); l2 != "" {
			street += " " + l2
		}
		if street != "" {
			h += " " + street
		}
	}
	return h
}

// routeMessage lays the stops out within Pushover's limit (the last ones go,
// and the message says the rest is on the receipts). carried < len(stops):
// the map link holds only the first stops.
func routeMessage(stops []RouteStop, carried int) (title, text string) {
	n := len(stops)
	title = fmt.Sprintf("Route · %d stop", n)
	if n != 1 {
		title += "s"
	}
	lines := make([]string, 0, n+1)
	for _, st := range stops {
		lines = append(lines, st.Head)
	}
	if carried < n {
		lines = append(lines, fmt.Sprintf("Map: the first %d stops", carried))
	}
	if utf8.RuneCountInString(strings.Join(lines, "\n")) > routeMsgMax {
		const more = "+ more on the receipts"
		for len(lines) > 1 && utf8.RuneCountInString(strings.Join(append(lines[:len(lines):len(lines)], more), "\n")) > routeMsgMax {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, more)
	}
	text = strings.Join(lines, "\n")
	if text == "" {
		text = "Route"
	}
	return title, text
}
