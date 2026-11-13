package order

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// LookSettings are the store's colours (2.4.3): the accent every Quibase
// tablet and the web UI are drawn in, and each ticket type's colour on
// Kitchen (the bar; the timer keeps green -> red). Colours are "#RRGGBB".
// Chime (2026-11-13) is the store's one-time chime: the sound every tablet
// plays once when an order it shows changes elsewhere (and for an
// auto-accepted order). A name Quibase knows ("bell", "ding", "beeps",
// "soft"); one it doesn't know plays its default.
type LookSettings struct {
	Accent  string      `json:"accent"`
	Tickets TicketLooks `json:"tickets"`
	Chime   string      `json:"chime"`
}

// TicketLooks: a kitchen ticket's colour by type (counter = dine-in /
// counter orders).
type TicketLooks struct {
	Delivery string `json:"delivery"`
	Pickup   string `json:"pickup"`
	Counter  string `json:"counter"`
}

// DefaultAccent is the brand lavender; DefaultTicketLook the owner's blue;
// DefaultChime the two-note bell.
const (
	DefaultAccent     = "#AC94F4"
	DefaultTicketLook = "#3B82F6"
	DefaultChime      = "bell"
)

// DefaultLook: lavender accent, every ticket blue, the bell.
func DefaultLook() LookSettings {
	return LookSettings{Accent: DefaultAccent,
		Tickets: TicketLooks{Delivery: DefaultTicketLook, Pickup: DefaultTicketLook, Counter: DefaultTicketLook},
		Chime:   DefaultChime}
}

var (
	hexColour = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
	chimeName = regexp.MustCompile(`^[a-z0-9_]{1,24}$`)
)

// Validate checks every colour is "#RRGGBB" and the chime is a name
// (1-24 of a-z, 0-9, _).
func (l LookSettings) Validate() error {
	if !chimeName.MatchString(l.Chime) {
		return fmt.Errorf("chime must be a sound name like %q", DefaultChime)
	}
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
