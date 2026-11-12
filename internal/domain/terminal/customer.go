package terminal

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode"
)

// The store's customers on the Pi (TERMINAL-PLAN 1d, contract §2.10j): a
// copy of main's (its change feed), looked up by phone at the terminals with
// no internet; customers added or changed at a terminal are saved here at
// once and sent to main as the floor event customer.upsert.

// EventCustomerUpsert is the floor event a terminal's customer change writes.
const EventCustomerUpsert = "customer.upsert"

// Journal actions for customers (no names or numbers in the detail).
const (
	ActionCustomerAdd  = "customer.add"
	ActionCustomerEdit = "customer.edit"
)

// MaxCustomerAddresses is how many addresses a customer keeps (main's limit).
const MaxCustomerAddresses = 20

// Errors of the customer routes.
var (
	ErrCustomerNotFound = errors.New("no such customer")
	ErrTooManyAddresses = errors.New("a customer keeps up to 20 addresses")
	ErrPhoneTaken       = errors.New("belongs to another customer")
)

// Customer is one of the store's customers as the Pi keeps it.
type Customer struct {
	ID          string            `json:"id"`
	Phone       string            `json:"phone"`
	FirstName   string            `json:"first_name"`
	LastName    string            `json:"last_name"`
	Name        string            `json:"name"`
	Notes       string            `json:"notes"`
	Source      string            `json:"source"` // web | counter | import ('' = made here, not at main yet)
	UpdatedAt   string            `json:"updated_at,omitempty"`
	LastOrderAt string            `json:"last_order_at,omitempty"`
	Pending     bool              `json:"pending"` // changed at a terminal, main hasn't sent it back yet
	Addresses   []CustomerAddress `json:"addresses"`
}

// CustomerAddress is a saved delivery address. Lat / Lng: where an order to
// it was located (never sent to main).
type CustomerAddress struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Line1     string   `json:"line1"`
	Line2     string   `json:"line2"`
	City      string   `json:"city"`
	State     string   `json:"state"`
	Zip       string   `json:"zip"`
	Notes     string   `json:"notes"`
	Default   bool     `json:"default"`
	CreatedAt string   `json:"created_at,omitempty"`
	Lat       *float64 `json:"-"`
	Lng       *float64 `json:"-"`
	Area      *Area    `json:"area,omitempty"`
}

// Area is an address's place against the store's delivery radius
// (TERMINAL-PLAN "Out-of-area deliveries"): in (≤ radius), near (≤ radius +
// margin: warn only), out (farther: a manager's PIN), unknown (no point yet).
type Area struct {
	State      string   `json:"state"`
	DistanceMi *float64 `json:"distance_mi,omitempty"`
}

// Area states.
const (
	AreaIn      = "in"
	AreaNear    = "near"
	AreaOut     = "out"
	AreaUnknown = "unknown"
)

// DefaultOutOfAreaMarginMi is the warn-only margin past the radius when main
// sends none (terminal setting out_of_area_margin_mi).
const DefaultOutOfAreaMarginMi = 1.0

const metersPerMile = 1609.344

// AreaOf places a point dist meters from the store against a radius and the
// warn-only margin (miles).
func AreaOf(distM float64, radiusM int64, marginMi float64) Area {
	mi := math.Round(distM/metersPerMile*10) / 10
	a := Area{DistanceMi: &mi}
	switch r := float64(radiusM); {
	case distM <= r:
		a.State = AreaIn
	case distM <= r+marginMi*metersPerMile:
		a.State = AreaNear
	default:
		a.State = AreaOut
	}
	return a
}

// DistanceM is the straight-line distance between two points (meters), as
// main measures the delivery radius.
func DistanceM(lat1, lng1, lat2, lng2 float64) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dLat, dLng := (lat2-lat1)*rad, (lng2-lng1)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(h)))
}

// NormPhone keeps a US phone's 10 digits ("" when it isn't one), main's rule.
func NormPhone(p string) string {
	d := Digits(p)
	if len(d) == 11 && d[0] == '1' {
		d = d[1:]
	}
	if len(d) != 10 || d[0] == '0' || d[0] == '1' {
		return ""
	}
	return d
}

// Digits keeps only the digits of s.
func Digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Reverse reverses a string of digits (phone_rev).
func Reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// JoinName is the full name for a first + last name (main's rule).
func JoinName(first, last string) string {
	return strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
}

// Clean collapses spaces and clips to n characters.
func Clean(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = strings.TrimSpace(string(r[:n]))
	}
	return s
}

// streetWords: main's address matching (customer.NormLine1).
var streetWords = map[string]string{
	"street": "st", "str": "st", "avenue": "ave", "av": "ave", "road": "rd", "drive": "dr", "lane": "ln",
	"court": "ct", "boulevard": "blvd", "place": "pl", "circle": "cir", "terrace": "ter", "parkway": "pkwy",
	"highway": "hwy", "square": "sq", "trail": "trl", "plaza": "plz", "pike": "pike",
	"north": "n", "south": "s", "east": "e", "west": "w",
	"northeast": "ne", "northwest": "nw", "southeast": "se", "southwest": "sw",
}

// NormLine1 is an address's first line for matching (main's rule): lower
// case, no punctuation, single spaces, common street words shortened.
func NormLine1(line1 string) string {
	f := strings.FieldsFunc(strings.ToLower(line1), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for i, w := range f {
		if s, ok := streetWords[w]; ok {
			f[i] = s
		}
	}
	return strings.Join(f, " ")
}

// Zip5 is the 5-digit zip ("" when there is none).
func Zip5(zip string) string {
	d := Digits(zip)
	if len(d) < 5 {
		return ""
	}
	return d[:5]
}

// SameAddress: the same normalised line 1 and 5-digit zip (a missing zip on
// either side matches on line 1 alone) — main's rule.
func SameAddress(line1a, zipa, line1b, zipb string) bool {
	la, lb := NormLine1(line1a), NormLine1(line1b)
	if la == "" || la != lb {
		return false
	}
	za, zb := Zip5(zipa), Zip5(zipb)
	return za == "" || zb == "" || za == zb
}

// FeedCustomer is one customer of main's change feed (contract/main.md §2.5zc).
type FeedCustomer struct {
	ID        string            `json:"id"`
	Phone     string            `json:"phone"`
	FirstName string            `json:"first_name"`
	LastName  string            `json:"last_name"`
	Name      string            `json:"name"`
	Notes     string            `json:"notes"`
	Source    string            `json:"source"`
	UpdatedAt time.Time         `json:"updated_at"`
	Addresses []CustomerAddress `json:"addresses"`
}

// FeedPage is one page of GET /store/v1/agent/customers.
type FeedPage struct {
	Customers []FeedCustomer `json:"customers"`
	Deleted   []string       `json:"deleted"`
	Cursor    string         `json:"cursor"`
	HasMore   bool           `json:"has_more"`
	Reset     bool           `json:"reset"`
}

// CustomerSync is the feed's saved state on the Pi.
type CustomerSync struct {
	LocationID string
	Cursor     string
	SyncedAt   time.Time // zero = never
}

// LocatedAddress is a delivery address main located for one of the Pi's orders.
type LocatedAddress struct {
	Line1, Zip string
	Lat, Lng   float64
}
