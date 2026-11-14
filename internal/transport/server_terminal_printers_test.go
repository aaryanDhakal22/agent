package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/device"
)

// The owner 2026-11-14: a counter terminal that lost its printer settings
// printed at 48 columns on a 42-column printer. The Pi keeps a copy per
// printer (contract §2.10ap) that the terminal reads back.
func TestTerminalPrinterProfiles(t *testing.T) {
	s, h, tabletKey, termKey, _ := setupServer(t)
	ctx := context.Background()

	// Nothing kept yet.
	rec := call(h, "GET", "/api/terminal/printer-profiles", termKey, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"profiles":[]`) {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}

	// A terminal's key alone keeps one (no staff login), its id / name the Pi's.
	put := `{"printer_key":"0483:5743:A1","profile":{"key":"0483:5743:A1","width":42,"columns":42,"kick":"escp_pin2","logo_ok":true},"route":{"backup":"Online"},"device_id":"forged"}`
	if rec := call(h, "PUT", "/api/terminal/printer-profile", termKey, put); rec.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "GET", "/api/terminal/printer-profiles", termKey, "")
	var got struct {
		Profiles []device.TerminalPrinter `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Profiles) != 1 {
		t.Fatalf("list: %v %s", err, rec.Body)
	}
	p := got.Profiles[0]
	if p.PrinterKey != "0483:5743:A1" || p.DeviceID != "term-1" || p.DeviceName != "Counter 1" || p.UpdatedAt.IsZero() {
		t.Fatalf("kept: %+v", p)
	}
	var prof map[string]any
	_ = json.Unmarshal(p.Profile, &prof)
	if prof["columns"] != float64(42) || prof["kick"] != "escp_pin2" || !strings.Contains(string(p.Route), `"backup":"Online"`) {
		t.Fatalf("profile / route: %s %s", p.Profile, p.Route)
	}

	// The same printer again: replaced, not a second copy.
	put2 := `{"printer_key":"0483:5743:A1","profile":{"key":"0483:5743:A1","width":48,"columns":48}}`
	if rec := call(h, "PUT", "/api/terminal/printer-profile", termKey, put2); rec.Code != http.StatusNoContent {
		t.Fatalf("put again: %d %s", rec.Code, rec.Body)
	}
	list, _ := s.terminalPrinters().TerminalPrinters(ctx)
	if len(list) != 1 || !strings.Contains(string(list[0].Profile), `"columns":48`) || len(list[0].Route) != 0 {
		t.Fatalf("replaced: %+v", list)
	}

	// Refused bodies.
	for _, b := range []string{
		`{"printer_key":"","profile":{"width":48}}`,
		`{"printer_key":"has space","profile":{"width":48}}`,
		`{"printer_key":"0483:5743","profile":"not an object"}`,
		`{"printer_key":"0483:5743","profile":{"width":48},"route":[1]}`,
		`{"printer_key":"0483:5743"}`,
		`not json`,
	} {
		if rec := call(h, "PUT", "/api/terminal/printer-profile", termKey, b); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", b, rec.Code, rec.Body)
		}
	}
	big := `{"printer_key":"0483:5743","profile":{"x":"` + strings.Repeat("a", 3*device.MaxTerminalPrinterJSON) + `"}}`
	if rec := call(h, "PUT", "/api/terminal/printer-profile", termKey, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too big: %d", rec.Code)
	}

	// A tablet's key can't (terminal routes).
	if rec := call(h, "GET", "/api/terminal/printer-profiles", tabletKey, ""); rec.Code != http.StatusForbidden {
		t.Errorf("tablet: %d %s", rec.Code, rec.Body)
	}

	// At most MaxTerminalPrinters copies: the oldest go.
	for i := 0; i < device.MaxTerminalPrinters+3; i++ {
		b := fmt.Sprintf(`{"printer_key":"0000:0000:192.168.1.%d:9100","profile":{"width":48}}`, i+10)
		if rec := call(h, "PUT", "/api/terminal/printer-profile", termKey, b); rec.Code != http.StatusNoContent {
			t.Fatalf("put %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	list, _ = s.terminalPrinters().TerminalPrinters(ctx)
	if len(list) != device.MaxTerminalPrinters {
		t.Fatalf("kept %d, want %d", len(list), device.MaxTerminalPrinters)
	}
	for _, p := range list {
		if p.PrinterKey == "0483:5743:A1" {
			t.Fatal("the oldest copy should have gone")
		}
	}
}

// The route is device-level: a driver logged in on the terminal still gets
// its printer settings back (driverRoutes), and nobody logged in at all too.
func TestTerminalPrinterProfileRouteRules(t *testing.T) {
	for _, p := range []string{"GET /api/terminal/printer-profiles", "PUT /api/terminal/printer-profile"} {
		if !driverRoutes[p] {
			t.Errorf("%s not in driverRoutes", p)
		}
		if staffRoutes[p] {
			t.Errorf("%s needs no staff login", p)
		}
	}
}
