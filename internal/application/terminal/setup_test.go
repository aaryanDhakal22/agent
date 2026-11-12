package terminal

import (
	"encoding/json"
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/terminal"
)

// Setup wizard (contract 2.10y): the readiness counts and the journal rows a
// logged-out terminal may write.
func TestSetupViewCounts(t *testing.T) {
	r := newRig(t)
	v := r.svc.SetupView([]RosterPrinter{{ID: pizzaPrn, Name: "Pizza"}, {ID: "prn-wings", Name: "Wings"}})
	if v.Menu == nil || v.Menu.Version != 12 || v.Menu.Items == 0 || v.Menu.FetchedAt == nil {
		t.Fatalf("menu: %+v", v.Menu)
	}
	if v.Routes == nil || len(v.Routes.Printers) != 2 || v.Routes.Printers[0].Items != 2 || v.Routes.Printers[1].Items != 0 {
		t.Fatalf("routes: %+v", v.Routes)
	}
	if v.Routes.Unrouted != v.Menu.Items-2 || len(v.Routes.UnknownPrinter) != 0 {
		t.Fatalf("unrouted: %+v (items %d)", v.Routes, v.Menu.Items)
	}
	// The staff counts: 2 cashiers + 1 manager with PINs (the fixture).
	if v.Staff == nil || v.Staff.Managers != 1 || v.Staff.Cashiers != 2 || v.Staff.Drivers != 0 {
		t.Fatalf("staff: %+v", v.Staff)
	}
	if v.CounterStart == nil || *v.CounterStart != 300 || v.AutoCloseAt == nil || *v.AutoCloseAt != terminal.DefaultAutoCloseAt || v.TaxBps == nil {
		t.Fatalf("settings: %+v", v)
	}
	// A route naming a printer the roster doesn't have.
	v = r.svc.SetupView([]RosterPrinter{{ID: "prn-wings", Name: "Wings"}})
	if len(v.Routes.UnknownPrinter) != 1 || v.Routes.UnknownPrinter[0] != "Pizza" {
		t.Fatalf("unknown printer: %+v", v.Routes)
	}
}

func TestSetupJournalWithoutALogin(t *testing.T) {
	r := newRig(t)
	for _, a := range []string{"setup.started", "setup.finished", "setup.skipped_by_admin", "setup.card_test", "admin_drawer_test", "drawer_kick", "admin_mode"} {
		e, err := r.svc.PostJournal(r.ctx, till, JournalIn{Action: a, Detail: json.RawMessage(`{"step":"printer"}`)})
		if err != nil || e.StaffID != "" || e.DeviceID != till.DeviceID {
			t.Fatalf("%s logged out: %+v %v", a, e, err)
		}
	}
	if _, err := r.svc.PostJournal(r.ctx, till, JournalIn{Action: "no_sale"}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("no_sale still needs a login: %v", err)
	}
	// The Pi's own setup rows can't be posted.
	for _, a := range []string{terminal.ActionPairingAdminCode, terminal.ActionTerminalPrint, terminal.ActionSetupCardRefund} {
		r.login(t, till, "1234")
		if _, err := r.svc.PostJournal(r.ctx, till, JournalIn{Action: a}); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%s is the Pi's: %v", a, err)
		}
	}
}
