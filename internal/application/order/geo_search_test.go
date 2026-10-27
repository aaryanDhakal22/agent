package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
)

// 2.2.2: main's corrected flag + note ride the order's geo (stored, reloaded,
// on the wire); a search's check carries up to 5 candidates.
func TestGeoCorrectedNoteAndSearchCandidates(t *testing.T) {
	ctx := context.Background()
	svc, repo, _, _, rpc := newIntegService(t)
	res := mustUpsert(t, svc, "400", extOrder("delivery", "a"))

	lat, lng := 39.3622852, -76.5820168
	so, err := svc.OnGeo(ctx, order.GeoUpdate{OrderID: res.OrderID, State: order.GeoLocated, Lat: &lat, Lng: &lng,
		Quality: "exact", Corrected: true})
	if err != nil || !so.Geo.Corrected || !so.Geo.Located() {
		t.Fatalf("located = %+v %v", so.Geo, err)
	}
	again, err := repo.GetByID(ctx, uuid.MustParse(res.OrderID))
	if err != nil || !again.Geo.Corrected {
		t.Fatalf("reloaded = %+v %v", again.Geo, err)
	}
	raw, _ := json.Marshal(again)
	if !strings.Contains(string(raw), `"corrected":true`) {
		t.Fatalf("wire: %s", raw)
	}

	// A staff search: check with 5 places (a 6th is dropped), no corrected.
	if err := svc.SearchAddress(ctx, res.OrderID, " 1613 Wadsworth   Way "); err != nil {
		t.Fatal(err)
	}
	if c := rpc.of(MsgGeoSearch); len(c) != 1 || c[0].Payload["order_id"] != res.OrderID || c[0].Payload["query"] != "1613 Wadsworth Way" {
		t.Fatalf("geo.search = %+v", c)
	}
	var cands []order.GeoCandidate
	for i := 0; i < 6; i++ {
		l := fmt.Sprintf("%d Wadsworth Way", 1610+i)
		cands = append(cands, order.GeoCandidate{Label: l, Line1: l, City: "Baltimore", State: "MD", PostalCode: "21239", Lat: 39.36, Lng: -76.58})
	}
	so, err = svc.OnGeo(ctx, order.GeoUpdate{OrderID: res.OrderID, State: order.GeoCheck, Candidates: cands, Corrected: true})
	if err != nil || len(so.Geo.Candidates) != order.MaxGeoCandidates || so.Geo.Corrected || so.Geo.Located() {
		t.Fatalf("check = %+v %v", so.Geo, err)
	}
	if err := svc.ChooseCandidate(ctx, res.OrderID, 4); err != nil {
		t.Fatalf("the 5th candidate can be chosen: %v", err)
	}

	// Nothing found: not_found with main's note.
	so, err = svc.OnGeo(ctx, order.GeoUpdate{OrderID: res.OrderID, State: order.GeoNotFound, Note: "no match for that search"})
	if err != nil || so.Geo.Note != "no match for that search" || len(so.Geo.Candidates) != 0 {
		t.Fatalf("not_found = %+v %v", so.Geo, err)
	}
	raw, _ = json.Marshal(so)
	if !strings.Contains(string(raw), `"note":"no match for that search"`) || strings.Contains(string(raw), `"corrected"`) {
		t.Fatalf("wire: %s", raw)
	}

	// Validation + main's answers.
	for _, q := range []string{"", "   ", strings.Repeat("a", order.MaxGeoSearchQuery+1)} {
		if err := svc.SearchAddress(ctx, res.OrderID, q); !errors.Is(err, ErrBadQuery) {
			t.Errorf("query %.10q: %v", q, err)
		}
	}
	if err := svc.SearchAddress(ctx, uuid.NewString(), "1 Main St"); !errors.Is(err, order.ErrNotFound) {
		t.Errorf("unknown order: %v", err)
	}
	rpc.reply = `{"ok":false,"code":"too_soon"}`
	if err := svc.SearchAddress(ctx, res.OrderID, "1 Main St"); !errors.Is(err, ErrTooSoon) {
		t.Errorf("too soon: %v", err)
	}
	svc.SetGeo(GeoDeps{Send: rpc.send, Linked: func() bool { return false }})
	if err := svc.SearchAddress(ctx, res.OrderID, "1 Main St"); !errors.Is(err, ErrMainOffline) {
		t.Errorf("unlinked: %v", err)
	}
}
