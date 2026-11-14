package device

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// TerminalPrinter is the Pi's copy of one counter terminal's receipt printer
// settings (contract §2.10ap): what the terminal's printer test and drawer
// setup learned, kept so a terminal that lost its own gets them back.
// Profile and Route are the terminal's JSON, kept as sent.
type TerminalPrinter struct {
	PrinterKey string          `json:"printer_key"`
	DeviceID   string          `json:"device_id"`
	DeviceName string          `json:"device_name"`
	Profile    json.RawMessage `json:"profile"`
	Route      json.RawMessage `json:"route,omitempty"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// TerminalPrinterStore keeps the copies (table terminal_printers).
type TerminalPrinterStore interface {
	TerminalPrinters(ctx context.Context) ([]TerminalPrinter, error)
	SetTerminalPrinter(ctx context.Context, p TerminalPrinter) error
}

// MaxTerminalPrinters bounds the copies kept (the oldest dropped).
const MaxTerminalPrinters = 32

// MaxTerminalPrinterJSON bounds a profile or a route (bytes).
const MaxTerminalPrinterJSON = 8 << 10

// MaxPrinterKey bounds a printer key (characters).
const MaxPrinterKey = 200

// ErrBadTerminalPrinter is a copy the Pi refuses (the reason in its text).
var ErrBadTerminalPrinter = errors.New("bad terminal printer")

// CheckTerminalPrinter answers nil for a copy the Pi keeps: a printer key of
// printable ASCII, a JSON object profile and an optional JSON object route,
// each within its size.
func CheckTerminalPrinter(p TerminalPrinter) error {
	k := strings.TrimSpace(p.PrinterKey)
	if k == "" || len(k) > MaxPrinterKey {
		return errors.Join(ErrBadTerminalPrinter, errors.New("printer_key is 1-200 characters"))
	}
	for _, r := range k {
		if r < 0x21 || r > 0x7E {
			return errors.Join(ErrBadTerminalPrinter, errors.New("printer_key is printable ASCII without spaces"))
		}
	}
	if !jsonObject(p.Profile) || len(p.Profile) > MaxTerminalPrinterJSON {
		return errors.Join(ErrBadTerminalPrinter, errors.New("profile is a JSON object of at most 8 KB"))
	}
	if len(p.Route) > 0 && string(p.Route) != "null" && (!jsonObject(p.Route) || len(p.Route) > MaxTerminalPrinterJSON) {
		return errors.Join(ErrBadTerminalPrinter, errors.New("route is a JSON object of at most 8 KB"))
	}
	return nil
}

func jsonObject(b json.RawMessage) bool {
	var m map[string]any
	return len(b) > 0 && json.Unmarshal(b, &m) == nil && m != nil
}
