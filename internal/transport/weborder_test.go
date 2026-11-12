package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	orderApp "quiccpos/agent/internal/application/order"
)

// The stage-3 routes (2.5.0): ADD TIP, CANCEL ORDER and the online-ordering
// proxies — auth, request checks and how each service error reaches Quibase.

func TestWebOrderRoutesNeedADevice(t *testing.T) {
	s, h, _ := diagServer(t)
	hub := integrationKey(t, s)
	id := uuid.NewString()
	for _, rt := range []struct{ method, path, body string }{
		{"POST", "/api/orders/" + id + "/tip", `{"tip_cents":100}`},
		{"POST", "/api/orders/" + id + "/cancel-order", `{"reason":"other"}`},
		{"GET", "/api/online", ""},
		{"POST", "/api/online/pause", `{"preset":"15m"}`},
		{"DELETE", "/api/online/pause", ""},
		{"GET", "/api/online/menu", ""},
		{"POST", "/api/online/stock", `{"kind":"item","id":"x","in_stock":false}`},
	} {
		if rec := call(h, rt.method, rt.path, "", rt.body); rec.Code != http.StatusUnauthorized || code(rec) != "device_required" {
			t.Errorf("%s %s without a key: %d %s", rt.method, rt.path, rec.Code, rec.Body)
		}
		if rec := call(h, rt.method, rt.path, hub, rt.body); rec.Code != http.StatusForbidden || code(rec) != "integration_only" {
			t.Errorf("%s %s with the hub's key: %d %s", rt.method, rt.path, rec.Code, rec.Body)
		}
	}
}

func TestTipAndCancelRequestChecks(t *testing.T) {
	_, h, key := diagServer(t)
	missing := uuid.NewString()
	for _, c := range []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"tip: bad id", "/api/orders/nope/tip", `{"tip_cents":100}`, 400, "bad_request"},
		{"tip: no tip_cents", "/api/orders/" + missing + "/tip", `{}`, 400, "bad_request"},
		{"tip: not json", "/api/orders/" + missing + "/tip", `tip`, 400, "bad_request"},
		{"tip: negative", "/api/orders/" + missing + "/tip", `{"tip_cents":-1}`, 400, "bad_request"},
		{"tip: over the cap", "/api/orders/" + missing + "/tip", `{"tip_cents":50001}`, 400, "bad_request"},
		{"tip: unknown order", "/api/orders/" + missing + "/tip", `{"tip_cents":100}`, 404, ""},
		{"cancel: bad id", "/api/orders/nope/cancel-order", `{"reason":"other"}`, 400, "bad_request"},
		{"cancel: bad reason", "/api/orders/" + missing + "/cancel-order", `{"reason":"because"}`, 400, "bad_request"},
		{"cancel: no reason", "/api/orders/" + missing + "/cancel-order", `{}`, 400, "bad_request"},
		{"cancel: note too long", "/api/orders/" + missing + "/cancel-order", `{"reason":"other","note":"` + strings.Repeat("é", 141) + `"}`, 400, "bad_request"},
		{"cancel: unknown order", "/api/orders/" + missing + "/cancel-order", `{"reason":"other"}`, 404, ""},
	} {
		rec := call(h, "POST", c.path, key, c.body)
		if rec.Code != c.status || (c.code != "" && code(rec) != c.code) {
			t.Errorf("%s: %d %s, want %d %s", c.name, rec.Code, rec.Body, c.status, c.code)
		}
	}
	// 140 characters (not bytes) is fine, spaces around it are trimmed: it
	// reaches the order lookup (404), not the note check.
	note := "  " + strings.Repeat("é", 140) + "  "
	if rec := call(h, "POST", "/api/orders/"+missing+"/cancel-order", key, `{"reason":"customer_asked","note":"`+note+`"}`); rec.Code != 404 {
		t.Errorf("140-character note: %d %s", rec.Code, rec.Body)
	}
}

// fakeMain answers the online-ordering RPCs like main would.
type fakeMain struct {
	status int
	reply  string
	err    error
	got    []string
}

func (f *fakeMain) send(_ context.Context, msgType string, payload any) (int, json.RawMessage, error) {
	b, _ := json.Marshal(payload)
	f.got = append(f.got, msgType+" "+string(b))
	if f.err != nil {
		return 0, nil, f.err
	}
	return f.status, json.RawMessage(f.reply), nil
}

func TestOnlineProxyAnswers(t *testing.T) {
	s, h, key := diagServer(t)
	// Not linked to main (no RPC hook yet): 503 main_unreachable.
	if rec := call(h, "GET", "/api/online", key, ""); rec.Code != 503 || code(rec) != "main_unreachable" {
		t.Fatalf("no main: %d %s", rec.Code, rec.Body)
	}
	m := &fakeMain{status: 200, reply: `{"state":"open","pause":null}`}
	linked := true
	s.service.SetGeo(orderApp.GeoDeps{Send: m.send, Linked: func() bool { return linked }})

	// 200: main's reply as is, for every route.
	for _, rt := range []struct{ method, path, body, sent string }{
		{"GET", "/api/online", "", `online.status {}`},
		{"POST", "/api/online/pause", `{"preset":"today","note":"  Oven down "}`, `online.pause {"note":"Oven down","preset":"today"}`},
		{"DELETE", "/api/online/pause", "", `online.resume {}`},
		{"GET", "/api/online/menu", "", `menu.stock {}`},
		{"POST", "/api/online/stock", `{"kind":"option","id":" o1 ","in_stock":true}`, `menu.stock.set {"hold":false,"id":"o1","in_stock":true,"kind":"option"}`},
	} {
		m.got = nil
		rec := call(h, rt.method, rt.path, key, rt.body)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != m.reply {
			t.Errorf("%s %s: %d %s", rt.method, rt.path, rec.Code, rec.Body)
		}
		if len(m.got) != 1 || m.got[0] != rt.sent {
			t.Errorf("%s %s sent %v, want %s", rt.method, rt.path, m.got, rt.sent)
		}
	}
	// Checked here before main is asked.
	m.got = nil
	for _, body := range []string{`{"preset":"forever"}`, `{}`, `x`} {
		if rec := call(h, "POST", "/api/online/pause", key, body); rec.Code != 400 || code(rec) != "bad_request" {
			t.Errorf("pause %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	for _, body := range []string{`{"kind":"size","id":"x","in_stock":false}`, `{"kind":"item","id":" ","in_stock":false}`, `{"kind":"item","id":"x"}`} {
		if rec := call(h, "POST", "/api/online/stock", key, body); rec.Code != 400 || code(rec) != "bad_request" {
			t.Errorf("stock %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if len(m.got) != 0 {
		t.Fatalf("bad requests reached main: %v", m.got)
	}

	// main's 400 (its message passes through), an older main (202 → 501),
	// main unreachable / refusing / garbage (503), unlinked (503).
	for _, c := range []struct {
		name   string
		status int
		reply  string
		err    error
		linked bool
		want   int
		code   string
		text   string
	}{
		{"main's 400", 400, `{"error":"note is too long"}`, nil, true, 400, "bad_request", "note is too long"},
		{"main's bare 400", 400, ``, nil, true, 400, "bad_request", "the server refused it"},
		{"older main", 202, ``, nil, true, 501, "main_unsupported", ""},
		{"no answer", 0, ``, errors.New("timeout"), true, 503, "main_unreachable", ""},
		{"main's 500", 500, `{"error":"boom"}`, nil, true, 503, "main_unreachable", ""},
		{"not json", 200, `oops`, nil, true, 503, "main_unreachable", ""},
		{"empty reply", 200, ``, nil, true, 503, "main_unreachable", ""},
		{"unlinked", 200, `{}`, nil, false, 503, "main_unreachable", ""},
	} {
		m.status, m.reply, m.err, linked = c.status, c.reply, c.err, c.linked
		rec := call(h, "GET", "/api/online", key, "")
		if rec.Code != c.want || code(rec) != c.code || !strings.Contains(rec.Body.String(), c.text) {
			t.Errorf("%s: %d %s, want %d %s %q", c.name, rec.Code, rec.Body, c.want, c.code, c.text)
		}
	}
}
