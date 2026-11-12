package transport

import (
	"context"
	"net/http"
	"testing"
	"time"

	printerApp "quiccpos/agent/internal/application/printer"
	domainprinter "quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/infra/printer/escpos"
	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
)

// The width test's answer (contract §2.3c "Paper width"): a ruler letter → the printer's width, saved in
// main over RPC printer.width (the round trip with main is pytest's test_printer_width).
func TestPrinterWidth_Answers(t *testing.T) {
	s, h, key := diagServer(t)
	meters, err := observability.NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory := func(name, ip string) domainprinter.Printer { return escpos.New(ip, name, zerolog.Nop()) }
	d := printerApp.NewDispatcher(ctx, factory, s.printers, nil, time.Hour, nil, zerolog.Nop(), meters)
	s.SetDiag(DiagDeps{Dispatcher: d, WidthTest: func(string, time.Time) []byte { return []byte("ruler") }})
	for _, c := range []struct {
		body   string
		status int
		code   string
	}{
		{`{}`, http.StatusBadRequest, "bad_request"},
		{`{"printer_id":"p1","letter":"0"}`, http.StatusBadRequest, "bad_letter"},
		{`{"printer_id":"p1","letter":"AB"}`, http.StatusBadRequest, "bad_letter"},
		{`{"printer_id":"p1","letter":"A"}`, http.StatusBadRequest, "too_narrow"}, // 31 characters
		{`{"printer_id":"p1","print_columns":40}`, http.StatusBadRequest, "bad_request"},
		{`{"printer_id":"nope","letter":"L"}`, http.StatusNotFound, "not_configured"},
	} {
		if rec := call(h, "POST", "/api/printers/width", key, c.body); rec.Code != c.status || code(rec) != c.code {
			t.Errorf("%s: %d %s", c.body, rec.Code, rec.Body)
		}
	}
	if rec := call(h, "POST", "/api/printers/width-test", key, `{"printer_id":"nope"}`); rec.Code != http.StatusNotFound {
		t.Errorf("width-test of an unknown printer: %d %s", rec.Code, rec.Body)
	}
	// A known printer on an unlinked Pi: the width can't be saved.
	d.Apply(ctx, []storeconfig.PrinterContext{{ID: "p1", Name: "Online", IP: "127.0.0.1:1", IsReceipt: true}})
	if rec := call(h, "POST", "/api/printers/width", key, `{"printer_id":"p1","letter":"L"}`); rec.Code != http.StatusServiceUnavailable || code(rec) != "main_offline" {
		t.Errorf("unlinked: %d %s", rec.Code, rec.Body)
	}
}

func TestWidthOfColumns(t *testing.T) {
	for read, want := range map[int]int{64: 48, 48: 48, 47: 42, 42: 42, 41: 32, 32: 32} {
		if got, ok := widthOfColumns(read); !ok || got != want {
			t.Errorf("widthOfColumns(%d) = %d %v, want %d", read, got, ok, want)
		}
	}
	if _, ok := widthOfColumns(31); ok {
		t.Error("31 columns accepted")
	}
}
