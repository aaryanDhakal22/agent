package transport

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/domain/order"
	domainprinter "quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/infra/printer/escpos"
	"quiccpos/agent/internal/infra/printer/receipt"
	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
)

// 2.3.0: driver reports any time — the print route, the clock-out print and
// the hub's driver-ledger PUT. Slips go to a fake raw-print printer.

// fakeTCPPrinter keeps every job's bytes that contains want.
type fakeTCPPrinter struct {
	addr string
	mu   sync.Mutex
	jobs []string
}

func newFakeTCPPrinter(t *testing.T, want string) *fakeTCPPrinter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeTCPPrinter{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
				b, _ := io.ReadAll(c)
				if strings.Contains(string(b), want) {
					f.mu.Lock()
					f.jobs = append(f.jobs, string(b))
					f.mu.Unlock()
				}
			}(c)
		}
	}()
	return f
}

func (f *fakeTCPPrinter) waitJobs(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := append([]string(nil), f.jobs...)
		f.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fake printer got fewer than %d slips", n)
	return nil
}

// withReportPrinting wires driver-report printing through a real dispatcher
// to a fake printer at addr ("" = a printer that isn't there).
func withReportPrinting(t *testing.T, s *Server, addr string) {
	t.Helper()
	meters, _ := observability.NewMeters()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := printerApp.NewDispatcher(ctx, func(name, ip string) domainprinter.Printer { return escpos.New(ip, name, zerolog.Nop()) },
		printerApp.NewRegistry(), nil, time.Hour, nil, zerolog.Nop(), meters)
	if addr == "" {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr = ln.Addr().String()
		ln.Close() // nothing listens there now
	}
	d.Apply(ctx, []storeconfig.PrinterContext{{ID: "p-front", Name: "Front", IP: addr, IsReceipt: true}})
	sc := storeconfig.StoreContext{Location: storeconfig.LocationContext{Name: "Lombardi's Pizza", Timezone: "America/New_York"}}
	s.service.SetDocumentPrinting(d.PrintDocument, func(rep order.DriverReport, from string) []byte {
		return receipt.BuildDriverReport(rep, sc, "", from, time.Now())
	})
}

func newDriver(t *testing.T, s *Server, name, pos string) *order.Driver {
	t.Helper()
	d, err := s.service.CreateDriver(context.Background(), name, pos)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.service.ClockIn(context.Background(), d.ID); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestClockOutPrintsTheDriversReport(t *testing.T) {
	s, h, key := diagServer(t)

	// No printers wired: clock-out as before, no report_printing.
	sam := newDriver(t, s, "Sam", "")
	rec := call(h, "POST", "/api/drivers/"+sam.ID+"/clock-out", key, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "report_printing") || !strings.Contains(rec.Body.String(), `"status":"off"`) {
		t.Fatalf("unwired clock-out = %d %s", rec.Code, rec.Body)
	}

	fp := newFakeTCPPrinter(t, "DRIVER REPORT")
	withReportPrinting(t, s, fp.addr)
	juma := newDriver(t, s, "Juma", "31")
	rec = call(h, "POST", "/api/drivers/"+juma.ID+"/clock-out", key, "")
	var ans struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		ReportPrinting bool   `json:"report_printing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ans); err != nil || rec.Code != http.StatusOK || ans.ID != juma.ID || ans.Status != "off" || !ans.ReportPrinting {
		t.Fatalf("clock-out = %d %s", rec.Code, rec.Body)
	}
	slips := fp.waitJobs(t, 1)
	if !strings.Contains(slips[0], "JUMA") || !strings.Contains(slips[0], "No deliveries this batch") {
		t.Fatalf("slip = %q", slips[0])
	}
	s.service.WaitBackground()
}

// The answer doesn't wait for the printer: a dead receipt printer still
// clocks the driver out at once (the slip's fate comes over SSE).
func TestClockOutDoesNotWaitForThePrinter(t *testing.T) {
	s, h, key := diagServer(t)
	withReportPrinting(t, s, "") // nothing listens: the print fails in the background
	juma := newDriver(t, s, "Juma", "31")
	start := time.Now()
	rec := call(h, "POST", "/api/drivers/"+juma.ID+"/clock-out", key, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"report_printing":true`) || !strings.Contains(rec.Body.String(), `"status":"off"`) {
		t.Fatalf("clock-out = %d %s", rec.Code, rec.Body)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("clock-out waited %s", time.Since(start))
	}
	s.service.WaitBackground()
}

func TestDriverReportRoutes(t *testing.T) {
	s, h, key := diagServer(t)
	juma := newDriver(t, s, "Juma", "31")
	base := "/api/drivers/" + juma.ID + "/report"
	for _, rt := range []struct{ method, path string }{{"GET", base}, {"POST", base + "/print"}} {
		if rec := call(h, rt.method, rt.path, "", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key = %d", rt.method, rt.path, rec.Code)
		}
	}
	if rec := call(h, "GET", base, key, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"driver_name":"Juma"`) {
		t.Fatalf("GET report = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/api/drivers/nope/report", key, ""); rec.Code != http.StatusNotFound || code(rec) != "driver_not_found" {
		t.Fatalf("unknown driver = %d %s", rec.Code, rec.Body)
	}
	// Not wired → 503.
	if rec := call(h, "POST", base+"/print", key, ""); rec.Code != http.StatusServiceUnavailable || code(rec) != "printing_unavailable" {
		t.Fatalf("print unwired = %d %s", rec.Code, rec.Body)
	}
	fp := newFakeTCPPrinter(t, "DRIVER REPORT")
	withReportPrinting(t, s, fp.addr)
	rec := call(h, "POST", base+"/print", key, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"printed_on":"Front"`) || !strings.Contains(rec.Body.String(), `"report":{`) {
		t.Fatalf("print = %d %s", rec.Code, rec.Body)
	}
	fp.waitJobs(t, 1)
	if rec := call(h, "POST", "/api/drivers/nope/report/print", key, ""); rec.Code != http.StatusNotFound || code(rec) != "driver_not_found" {
		t.Fatalf("print unknown = %d %s", rec.Code, rec.Body)
	}
	// Nothing printed → 502.
	withReportPrinting(t, s, "")
	if rec := call(h, "POST", base+"/print", key, ""); rec.Code != http.StatusBadGateway || code(rec) != "nothing_printed" {
		t.Fatalf("print, printer off = %d %s", rec.Code, rec.Body)
	}
}

func TestDriverLedgerRoute(t *testing.T) {
	s, h, tablet := diagServer(t)
	hub := integrationKey(t, s)
	body := `{"operation_day":"2026-10-26","generated_at":"2026-10-26T22:00:00Z","checks":[{"check":"1277130","number":57,"driver_pos_number":"31","closed":true,"check_at":"2026-10-26T21:00:00Z","paid":true,"tip_cents":300,"total_cents":2400,"card_cents":2700}],"unknown_pos_numbers":[]}`
	if rec := call(h, "PUT", "/api/integration/driver-ledger", tablet, body); rec.Code != http.StatusForbidden || code(rec) != "integration_required" {
		t.Fatalf("tablet = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "PUT", "/api/integration/driver-ledger", hub, body); rec.Code != http.StatusNoContent {
		t.Fatalf("hub = %d %s", rec.Code, rec.Body)
	}
	bad := `{"checks":[{"check":"1","total_cents":-5}]}`
	if rec := call(h, "PUT", "/api/integration/driver-ledger", hub, bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad ledger = %d %s", rec.Code, rec.Body)
	}
}
