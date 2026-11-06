package order

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// LookSettings are the store's colours (2.4.3): the accent every Quibase
// tablet and the web UI are drawn in, and each ticket type's colour on
// Kitchen (the bar; the timer keeps green -> red). Colours are "#RRGGBB".
type LookSettings struct {
	Accent  string      `json:"accent"`
	Tickets TicketLooks `json:"tickets"`
}

// TicketLooks: a kitchen ticket's colour by type (counter = dine-in /
// counter orders).
type TicketLooks struct {
	Delivery string `json:"delivery"`
	Pickup   string `json:"pickup"`
	Counter  string `json:"counter"`
}

// DefaultAccent is the brand lavender; DefaultTicketLook the owner's blue.
const (
	DefaultAccent     = "#AC94F4"
	DefaultTicketLook = "#3B82F6"
)

// DefaultLook: lavender accent, every ticket blue.
func DefaultLook() LookSettings {
	return LookSettings{Accent: DefaultAccent,
		Tickets: TicketLooks{Delivery: DefaultTicketLook, Pickup: DefaultTicketLook, Counter: DefaultTicketLook}}
}

var hexColour = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// Validate checks every colour is "#RRGGBB".
func (l LookSettings) Validate() error {
	for name, v := range map[string]string{"accent": l.Accent, "tickets.delivery": l.Tickets.Delivery,
		"tickets.pickup": l.Tickets.Pickup, "tickets.counter": l.Tickets.Counter} {
		if !hexColour.MatchString(v) {
			return fmt.Errorf("%s must be a colour like #3B82F6", name)
		}
	}
	return nil
}

// DecodeLook reads stored colours over the defaults.
func DecodeLook(raw []byte) (LookSettings, error) {
	l := DefaultLook()
	if len(raw) == 0 {
		return l, nil
	}
	err := json.Unmarshal(raw, &l)
	return l, err
}
