// Package printroute is the store's printer routing + emergency reroute
// setting (the owner, 2026-11-12; contract §2.3c, spec §4.42): where a ticket
// goes when the printer it was meant for can't print. It is a Pi setting
// (settings.print_routing), mirrored to main inside agent.state so a re-imaged
// Pi gets it back when it is linked again.
//
// The dispatcher tries, for a ticket aimed at printer T:
//
//  1. T itself;
//  2. the rule's targets for T (Targets), in order: mode "one" = the one
//     emergency printer, mode "per_printer" = T's own list, mode "none" =
//     nothing;
//  3. the automatic failover (the receipt printer, then the rest in roster
//     order).
//
// Printers the health check last saw down are moved behind every printer
// that is up (rule targets still first among them), so a dead stand-in
// doesn't cost a dial timeout per ticket.
package printroute

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Modes.
const (
	// ModeNone: no rule, only the automatic failover (the behaviour before
	// this setting existed, and the default).
	ModeNone = "none"
	// ModeOne: one emergency printer stands in for every other printer.
	ModeOne = "one"
	// ModePerPrinter: each printer has its own ordered list of stand-ins.
	ModePerPrinter = "per_printer"
)

// MaxTargets caps one printer's list (a store has a handful of printers; the
// cap only keeps a bad body small).
const MaxTargets = 8

// Routing is the setting. Both the emergency printer and the per-printer
// lists are kept whatever the mode, so switching modes back and forth loses
// nothing; only the mode's part is used.
type Routing struct {
	Mode string `json:"mode"`
	// EmergencyPrinterID (mode "one"): the store_config printer id that
	// prints for any printer that can't.
	EmergencyPrinterID string `json:"emergency_printer_id"`
	// Targets (mode "per_printer"): printer id -> the ids to try, in order,
	// when it can't print.
	Targets map[string][]string `json:"targets"`
}

// Default is mode "none".
func Default() Routing { return Routing{Mode: ModeNone, Targets: map[string][]string{}} }

// Decode reads a stored value; "" (never set) is the default.
func Decode(raw []byte) (Routing, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return Default(), nil
	}
	var r Routing
	if err := json.Unmarshal(raw, &r); err != nil {
		return Default(), err
	}
	return r.Normalize(), nil
}

// Normalize trims ids, drops empty ones, duplicates and a printer listed for
// itself, drops empty lists and fills an empty mode with "none".
func (r Routing) Normalize() Routing {
	out := Routing{Mode: strings.TrimSpace(r.Mode), EmergencyPrinterID: strings.TrimSpace(r.EmergencyPrinterID), Targets: map[string][]string{}}
	if out.Mode == "" {
		out.Mode = ModeNone
	}
	for from, list := range r.Targets {
		from = strings.TrimSpace(from)
		if from == "" {
			continue
		}
		seen := map[string]bool{from: true}
		var ids []string
		for _, id := range list {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			out.Targets[from] = ids
		}
	}
	return out
}

// Errors from Validate (the HTTP layer answers 400 with Code).
var ErrInvalid = errors.New("invalid printer routing")

// ValidationError is a 400 with a code the web page can show.
type ValidationError struct {
	Code   string
	Reason string
}

func (e *ValidationError) Error() string   { return e.Reason }
func (e *ValidationError) Is(t error) bool { return t == ErrInvalid }

// Validate checks a normalized setting against the printers the Pi has now
// (store_config ids): a known mode, an emergency printer for mode "one", and
// every id it names in the roster. Validate before Normalize would miss a
// printer listed for itself, so call it on Normalize's result.
func (r Routing) Validate(known map[string]bool) error {
	bad := func(code, format string, a ...any) error {
		return &ValidationError{Code: code, Reason: fmt.Sprintf(format, a...)}
	}
	switch r.Mode {
	case ModeNone, ModeOne, ModePerPrinter:
	default:
		return bad("bad_mode", "mode must be none, one or per_printer")
	}
	if r.Mode == ModeOne && r.EmergencyPrinterID == "" {
		return bad("no_emergency_printer", "choose the emergency printer")
	}
	if r.EmergencyPrinterID != "" && !known[r.EmergencyPrinterID] {
		return bad("unknown_printer", "the emergency printer isn't one of this store's printers")
	}
	for from, list := range r.Targets {
		if !known[from] {
			return bad("unknown_printer", "a reroute rule names a printer this store doesn't have")
		}
		if len(list) > MaxTargets {
			return bad("too_many_targets", "at most %d stand-ins per printer", MaxTargets)
		}
		for _, id := range list {
			if !known[id] {
				return bad("unknown_printer", "a reroute rule names a printer this store doesn't have")
			}
		}
	}
	return nil
}

// RuleTargets are the stand-ins the rule names for a ticket aimed at printer
// from, in order (never from itself). Ids the Pi no longer has are left to
// the caller to skip.
func (r Routing) RuleTargets(from string) []string {
	switch r.Mode {
	case ModeOne:
		if r.EmergencyPrinterID != "" && r.EmergencyPrinterID != from {
			return []string{r.EmergencyPrinterID}
		}
	case ModePerPrinter:
		var out []string
		for _, id := range r.Targets[from] {
			if id != from {
				out = append(out, id)
			}
		}
		return out
	}
	return nil
}
