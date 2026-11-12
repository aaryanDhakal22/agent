package order

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
)

// 00019 through the service: SSE order.changed, a check shown later as new
// (with the chime), void tickets only behind the POS switch, OK, finishing,
// TEST CALLOUT, settings validation.

func TestFinishDueUsesTheSetting(t *testing.T) {
	ctx := context.Background()
	svc, repo, events, _, _ := newIntegService(t)
	snap := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "test",
		SourceEventID: "p1", PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: "pickup"}}
	if _, err := repo.UpsertArrived(ctx, snap); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.ClaimAccept(ctx, uuid.MustParse(snap.ID), 20, false)
	_ = repo.RecordAccepted(ctx, *got)
	if _, err := svc.Ready(ctx, snap.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.FinishDue(ctx, time.Now()); n != 0 {
		t.Fatal("finished before its time")
	}
	if n, _ := svc.FinishDue(ctx, time.Now().Add(31*time.Minute)); n != 1 {
		t.Fatal("not finished after 30 min (default)")
	}
	if _, ok := eventOf(events, "order.updated"); !ok {
		t.Fatal("finish not published")
	}
	// 0 = never.
	if _, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { s.Finishing.OnlinePickupMinutes = 0; return nil }); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.FinishDue(ctx, time.Now().Add(10*time.Hour)); n != 0 || err != nil {
		t.Fatalf("finish with 0 = %d %v", n, err)
	}
}

func TestCalloutTestAndSettingsValidation(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	for _, bad := range []struct {
		kind   string
		number int
		voice  string
	}{{"test", 0, ""}, {"test", 10000, ""}, {"shout", 5, ""}, {"preview", 5, "../etc"}} {
		if _, err := svc.CalloutTest(ctx, bad.kind, bad.number, bad.voice); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%+v: err = %v", bad, err)
		}
	}
	res, err := svc.CalloutTest(ctx, "", 213, "kokoro:af_bella")
	if err != nil || res.ID == "" || res.HubConnected {
		t.Fatalf("test = %+v %v", res, err)
	}
	evs := feedOf(t, svc)
	last := evs[len(evs)-1]
	var d order.CalloutTestData
	_ = json.Unmarshal(last.Data, &d)
	if last.Type != order.FeedCalloutTest || d.Number != 213 || d.Kind != order.CalloutTest || d.Voice != "kokoro:af_bella" {
		t.Fatalf("feed = %s %s", last.Type, last.Data)
	}
	for _, change := range []func(*order.IntegrationSettings){
		func(s *order.IntegrationSettings) { s.Callout.Volume = 101 },
		func(s *order.IntegrationSettings) { s.Callout.MusicDip = -1 },
		func(s *order.IntegrationSettings) { s.Callout.RestoreSeconds = 11 },
		func(s *order.IntegrationSettings) { s.Finishing.OnlinePickupMinutes = 241 },
		func(s *order.IntegrationSettings) {
			s.Callout.VoiceLines = map[string]string{"kokoro:am_onyx": strings.Repeat("x", 161)}
		},
		func(s *order.IntegrationSettings) {
			s.Callout.VoiceLines = map[string]string{"kokoro:am_onyx": "two\nlines"}
		},
		func(s *order.IntegrationSettings) { s.Callout.VoiceLines = map[string]string{"bad voice": "hi <x>"} },
		func(s *order.IntegrationSettings) { s.Callout.GuestChance = 51 },
		func(s *order.IntegrationSettings) { s.Callout.GuestVoices = "a,b,c,d,e,f,g,h,i,j,k" },
		func(s *order.IntegrationSettings) { s.Callout.GuestVoices = "eleven labs:x" },
	} {
		if _, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { change(s); return nil }); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("bad settings saved: %v", err)
		}
	}
	if got := svc.IntegrationSettings().Callout; !reflect.DeepEqual(got, order.DefaultCallout()) {
		t.Fatalf("callout changed by a refused save: %+v", got)
	}
}

func TestCalloutVoiceLinesAndGuestSettings(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	if got := svc.IntegrationSettings().Callout; len(got.VoiceLines) != 0 || got.GuestChance != 0 {
		t.Fatalf("defaults: %+v", got)
	}
	lines := map[string]string{"elevenlabs:abc123": "This is Narrator and order number <x> is ready", "kokoro:am_onyx": "Hot and fresh!"}
	_, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error {
		s.Callout.VoiceLines = lines
		s.Callout.GuestChance, s.Callout.GuestVoices = 50, "elevenlabs:abc123,kokoro:am_onyx"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.IntegrationSettings().Callout; !reflect.DeepEqual(got.VoiceLines, lines) || got.GuestChance != 50 {
		t.Fatalf("saved: %+v", got)
	}
}
