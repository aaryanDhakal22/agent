package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/settingslog"
)

// memSettingsLog is an in-memory settingslog.Repository.
type memSettingsLog struct {
	mu sync.Mutex
	es []settingslog.Entry
}

func (m *memSettingsLog) Add(_ context.Context, e settingslog.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = int64(len(m.es) + 1)
	m.es = append(m.es, e)
	return nil
}

func (m *memSettingsLog) List(_ context.Context, since time.Time, limit int) ([]settingslog.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []settingslog.Entry{}
	for i := len(m.es) - 1; i >= 0 && len(out) < limit; i-- {
		if !m.es[i].At.Before(since) {
			out = append(out, m.es[i])
		}
	}
	return out, nil
}

func (m *memSettingsLog) all() []settingslog.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]settingslog.Entry(nil), m.es...)
}

func TestSettingsChangesAreLoggedWithTheDevice(t *testing.T) {
	s, h, key := diagServer(t)
	ring := devlog.New(0)
	s.SetDevLog(ring)
	hist := &memSettingsLog{}
	s.SetSettingsLog(hist)

	// A tablet turns online card tickets off: one entry, by the tablet's name.
	if rec := call(h, "PUT", "/api/settings/tickets", key, `{"online_card":false}`); rec.Code != 200 {
		t.Fatalf("tickets: %d %s", rec.Code, rec.Body)
	}
	es := hist.all()
	if len(es) != 1 || es[0].Section != "tickets" || es[0].By != (settingslog.By{Kind: "tablet", Name: "Counter", DeviceID: "dev-1"}) ||
		len(es[0].Changes) != 1 || es[0].Changes[0] != (settingslog.Change{Key: "online_card", From: true, To: false}) {
		t.Fatalf("entry: %+v", es)
	}
	ev, _ := ring.Since(0, 0)
	if len(ev) != 1 || ev[0].Kind != devlog.KindSettings || ev[0].Text != "Kitchen tickets: online_card on → off (tablet Counter)" {
		t.Fatalf("dev event: %+v", ev)
	}

	// The same value again, and a rejected PUT, change nothing.
	if rec := call(h, "PUT", "/api/settings/tickets", key, `{"online_card":false}`); rec.Code != 200 {
		t.Fatalf("same: %d", rec.Code)
	}
	if rec := call(h, "PUT", "/api/settings/times", key, `{"default_pickup_minutes":999}`); rec.Code != 400 {
		t.Fatalf("bad times: %d %s", rec.Code, rec.Body)
	}
	if n := len(hist.all()); n != 1 {
		t.Fatalf("entries after no-ops: %d", n)
	}

	// The Pi web page (a manager session) is named as such.
	cookie := httptest.NewRecorder()
	s.session.issue(cookie)
	req := httptest.NewRequest("PUT", "/api/settings/auto-accept", strings.NewReader(`{"auto_accept":true}`))
	for _, c := range cookie.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("web auto-accept: %d %s", rec.Code, rec.Body)
	}
	es = hist.all()
	if len(es) != 2 || es[1].Section != "auto-accept" || es[1].By != (settingslog.By{Kind: "web", Name: "Pi web page"}) {
		t.Fatalf("web entry: %+v", es)
	}
	ev, _ = ring.Since(1, 0)
	if len(ev) != 1 || ev[0].Text != "Auto-accept: off → on (Pi web page)" {
		t.Fatalf("web event: %+v", ev)
	}

	// Floor prefs and the clock-out time (session routes) are kept too; the
	// clock-out's last_run stamp is not a change.
	put := func(path, body string) {
		t.Helper()
		req := httptest.NewRequest("PUT", path, strings.NewReader(body))
		for _, c := range cookie.Result().Cookies() {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	put("/api/settings/floor", `{"green_till_minutes":25,"tip_tax_percent":6}`)
	put("/api/settings/driver-reset", `{"time":"09:30"}`)
	es = hist.all()
	if len(es) != 4 || es[2].Section != "floor" || len(es[2].Changes) != 1 || es[2].Changes[0].Key != "green_till_minutes" ||
		es[3].Section != "driver-reset" || len(es[3].Changes) != 1 || es[3].Changes[0].Key != "time" || es[3].Changes[0].To != "09:30" {
		t.Fatalf("session sections: %+v", es)
	}

	// GET /api/settings/changes: newest first, device auth.
	if rec := call(h, "GET", "/api/settings/changes", "", ""); rec.Code != 401 {
		t.Fatalf("no key: %d", rec.Code)
	}
	rec = call(h, "GET", "/api/settings/changes?limit=2", key, "")
	var out struct {
		Changes  []settingslog.Entry `json:"changes"`
		KeepDays int                 `json:"keep_days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatalf("changes: %d %s", rec.Code, rec.Body)
	}
	if len(out.Changes) != 2 || out.Changes[0].Section != "driver-reset" || out.Changes[1].Section != "floor" || out.KeepDays != 90 {
		t.Fatalf("changes: %s", rec.Body)
	}
	if rec := call(h, "GET", "/api/settings/changes?limit=x", key, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit: %d", rec.Code)
	}
}

func TestSettingsChangesWithoutAStore(t *testing.T) {
	_, h, key := diagServer(t)
	rec := call(h, "GET", "/api/settings/changes", key, "")
	if rec.Code != 200 || rec.Body.String() != `{"changes":[],"keep_days":90}`+"\n" {
		t.Fatalf("no store: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "PUT", "/api/settings/tickets", key, `{"online_card":false}`); rec.Code != 200 {
		t.Fatalf("tickets still save: %d", rec.Code)
	}
}
