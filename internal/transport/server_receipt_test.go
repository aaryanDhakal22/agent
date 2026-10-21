package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
)

type receiptAnswer struct {
	Prefs           order.ReceiptPrefs  `json:"prefs"`
	Defaults        *order.ReceiptPrefs `json:"defaults"`
	FooterEffective string              `json:"footer_effective"`
	FooterDefault   string              `json:"footer_store_default"`
	LocationFooter  string              `json:"location_footer"`
}

func decodeReceipt(t *testing.T, body []byte) receiptAnswer {
	t.Helper()
	var a receiptAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return a
}

func TestReceiptRoutesNeedADevice(t *testing.T) {
	_, h, _ := diagServer(t)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/settings/receipt"}, {"PUT", "/api/settings/receipt"}, {"POST", "/api/printers/sample"},
	} {
		if rec := call(h, rt.method, rt.path, "", "{}"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key: %d", rt.method, rt.path, rec.Code)
		}
	}
}

func TestReceiptPrefsGetPut(t *testing.T) {
	s, h, key := diagServer(t)
	rec := call(h, "GET", "/api/settings/receipt", key, "")
	a := decodeReceipt(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || a.Prefs != order.DefaultReceiptPrefs() || a.Defaults == nil ||
		*a.Defaults != order.DefaultReceiptPrefs() || a.FooterEffective != "ONLINE ORDER" || a.FooterDefault != "ONLINE ORDER" {
		t.Fatalf("GET defaults: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"footer_text":null`) {
		t.Errorf("an unset footer is null: %s", rec.Body)
	}

	// The location footer is the fallback before ONLINE ORDER.
	s.SetDiag(DiagDeps{StoreCtx: func() storeconfig.StoreContext {
		sc := storeconfig.StoreContext{}
		sc.Location.Config = map[string]any{"receipt_footer": "THANKS"}
		return sc
	}})
	if a := decodeReceipt(t, call(h, "GET", "/api/settings/receipt", key, "").Body.Bytes()); a.FooterEffective != "THANKS" || a.FooterDefault != "THANKS" || a.LocationFooter != "THANKS" {
		t.Fatalf("location footer: %+v", a)
	}

	rec = call(h, "PUT", "/api/settings/receipt", key, `{"kitchen_item_size":"xlarge","footer_text":"  THANK YOU ","copies":2,"show_phone":false}`)
	a = decodeReceipt(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || a.Prefs.KitchenItemSize != order.SizeXLarge || a.Prefs.Copies != 2 || a.Prefs.ShowPhone ||
		!a.Prefs.ExtraPaper || a.FooterEffective != "THANK YOU" || a.FooterDefault != "THANKS" {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if p := s.service.PrintPrefs(); p.Copies != 2 || p.ShowPhone {
		t.Errorf("printing uses the new settings at once: %+v", p)
	}
	if a := decodeReceipt(t, call(h, "GET", "/api/settings/receipt", key, "").Body.Bytes()); a.Prefs.KitchenItemSize != order.SizeXLarge || a.Prefs.FooterText == nil || *a.Prefs.FooterText != "THANK YOU" {
		t.Fatalf("GET after PUT: %+v", a)
	}
	// "" = no footer at all.
	if a := decodeReceipt(t, call(h, "PUT", "/api/settings/receipt", key, `{"footer_text":""}`).Body.Bytes()); a.FooterEffective != "" {
		t.Errorf("empty footer: %+v", a)
	}

	for body, reason := range map[string]string{
		`{"copies":3}`:                                      "copies must be 1 or 2",
		`{"kitchen_number_size":"normal"}`:                  "kitchen_number_size must be large, xlarge or huge",
		`{"footer_text":"` + strings.Repeat("X", 33) + `"}`: "footer_text must be at most 32 characters",
		`{"footer_text":"CAFÉ"}`:                            "footer_text may only use",
		`{"copies":"two"}`:                                  "invalid body",
		``:                                                  "JSON object",
	} {
		rec := call(h, "PUT", "/api/settings/receipt", key, body)
		var e struct{ Error, Code string }
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != http.StatusBadRequest || e.Code != "bad_request" || !strings.Contains(e.Error, reason) {
			t.Errorf("PUT %s: %d %s", body, rec.Code, rec.Body)
		}
	}
}

func TestPrinterSampleNoPrinterOrBusy(t *testing.T) {
	s, h, key := diagServer(t)
	if rec := call(h, "POST", "/api/printers/sample", key, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no dispatcher: %d %s", rec.Code, rec.Body)
	}
	meters, err := observability.NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.SetDiag(DiagDeps{Dispatcher: printerApp.NewDispatcher(ctx, nil, s.printers, nil, time.Hour, nil, zerolog.Nop(), meters)})
	s.SetReceiptSample(func(order.ReceiptPrefs) []byte { return []byte("sample") })
	for _, body := range []string{"", `{"printer_id":"nope"}`} {
		if rec := call(h, "POST", "/api/printers/sample", key, body); rec.Code != http.StatusNotFound || code(rec) != "not_configured" {
			t.Errorf("%q: %d %s", body, rec.Code, rec.Body)
		}
	}
}

func TestSamplePrinterChoice(t *testing.T) {
	roster := []printerApp.RosterEntry{{ID: "pizza"}, {ID: "front", IsReceipt: true}, {ID: "fry"}}
	for want, got := range map[string]string{
		"front": samplePrinter(roster, ""),
		"fry":   samplePrinter(roster, "fry"),
		"":      samplePrinter(roster, "gone"),
	} {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	if samplePrinter(roster[:1], "") != "pizza" || samplePrinter(nil, "") != "" {
		t.Error("no receipt printer = the first; none = \"\"")
	}
}
