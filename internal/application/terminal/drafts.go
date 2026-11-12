package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// Shared drafts: hold + TAKE OVER (TERMINAL-PLAN "Suspend / hold", "Lock
// mid-order"; contract §2.10i). SUSPEND / the idle lock parks an order on the
// Pi (held); RESUME opens it on one terminal (the holder); the other terminal
// gets 409 open_elsewhere until it sends take_over. SQLite (they survive a
// restart), per store, cleared at Close batch, never sent to main.

// DraftRepo is the drafts' storage (repositories.OrderRepository).
type DraftRepo interface {
	TerminalDrafts(ctx context.Context, locationID string) ([]terminal.Draft, error)
	PutTerminalDraft(ctx context.Context, in terminal.Draft, put terminal.DraftPut, now time.Time) (terminal.Draft, *terminal.Holder, error)
	DeleteTerminalDraft(ctx context.Context, id, locationID, deviceID string) (bool, error)
}

// DraftList is GET /api/terminal/drafts' answer.
type DraftList struct {
	Drafts []terminal.Draft `json:"drafts"`
}

// DraftIn is PUT /api/terminal/drafts/{id}.
type DraftIn struct {
	Type     string          `json:"type"`
	Name     string          `json:"name"`
	Held     bool            `json:"held"`
	OrderID  string          `json:"order_id"`
	TakeOver bool            `json:"take_over"`
	Draft    json.RawMessage `json:"draft"`
}

// DraftResult is PUT /api/terminal/drafts/{id}'s answer.
type DraftResult struct {
	Draft terminal.Draft `json:"draft"`
}

func (s *Service) location() string {
	_, loc := s.orders.StoreIDs()
	return loc
}

// Drafts are the store's shared drafts, oldest first (no login needed: the
// Pickup list shows them).
func (s *Service) Drafts(ctx context.Context) (*DraftList, error) {
	list, err := s.repo.TerminalDrafts(ctx, s.location())
	if err != nil {
		return nil, err
	}
	return &DraftList{Drafts: list}, nil
}

// PutDraft creates or replaces a draft. The holder (device + staff) comes
// from the key and the login: held = nobody has it open, not held = open on
// this terminal. Open on another terminal → *terminal.OpenElsewhere unless
// take_over (the old holder is told terminal.draft.taken {id, by}).
func (s *Service) PutDraft(ctx context.Context, a Actor, id string, in DraftIn) (*DraftResult, error) {
	st, _, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if !keyRe.MatchString(id) {
		return nil, badRequest("a draft id is 8-64 letters, digits, _ . : -")
	}
	if _, err := checkType(in.Type); err != nil || in.Type == "" {
		return nil, badRequest("type must be counter, pickup or delivery")
	}
	name := strings.TrimSpace(in.Name)
	if utf8.RuneCountInString(name) > terminal.MaxDraftName {
		return nil, badRequest("name is at most 80 characters")
	}
	if in.OrderID != "" {
		if _, err := uuid.Parse(in.OrderID); err != nil {
			return nil, badRequest("order_id must be a uuid")
		}
	}
	d := in.Draft
	if len(d) == 0 || string(d) == "null" {
		d = json.RawMessage("{}")
	}
	if len(d) > terminal.MaxDraftBytes {
		return nil, badRequest("draft is at most 64 KB")
	}
	var obj map[string]any
	if err := json.Unmarshal(d, &obj); err != nil {
		return nil, badRequest("draft must be a JSON object")
	}
	by := terminal.Holder{DeviceID: a.DeviceID, DeviceName: a.DeviceName, StaffID: st.ID, StaffName: st.Name()}
	now := s.now().UTC().Truncate(time.Millisecond)
	got, taken, err := s.repo.PutTerminalDraft(ctx, terminal.Draft{ID: id, LocationID: s.location(), Type: in.Type, Name: name,
		Held: in.Held, OrderID: in.OrderID, Draft: d}, terminal.DraftPut{By: by, Held: in.Held, TakeOver: in.TakeOver}, now)
	var oe *terminal.OpenElsewhere
	if errors.As(err, &oe) && oe.TakeOverRefused { // the store wins (§2.10ab)
		s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDraftTakeOverRefused,
			Detail: detail(map[string]any{"draft_id": id, "holder_device_id": oe.Holder.DeviceID, "holder_staff_id": oe.Holder.StaffID})})
		s.logger.Info().Ctx(ctx).Str("draft_id", id).Str("device_id", a.DeviceID).Str("holder_device_id", oe.Holder.DeviceID).
			Msg("Draft take-over refused: a store terminal holds it")
	}
	if err != nil {
		return nil, err
	}
	if taken != nil {
		// device_id: the terminal (or call-center seat) it was taken from.
		s.publish(terminal.EventDraftTaken, map[string]any{"id": id, "by": got.Holder, "from": taken, "device_id": taken.DeviceID})
		s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDraftTakeOver,
			Detail: detail(map[string]any{"draft_id": id, "from_device_id": taken.DeviceID, "from_staff_id": taken.StaffID})})
		s.logger.Info().Ctx(ctx).Str("draft_id", id).Str("device_id", a.DeviceID).Str("from_device_id", taken.DeviceID).Msg("Draft taken over")
	}
	s.publish(terminal.EventDraftChanged, map[string]any{"draft": got})
	return &DraftResult{Draft: got}, nil
}

// DeleteDraft removes a draft (sent, paid or cleared); open on another
// terminal → *terminal.OpenElsewhere. A missing one is fine.
func (s *Service) DeleteDraft(ctx context.Context, a Actor, id string) error {
	if _, _, err := s.staffOf(a); err != nil {
		return err
	}
	if !keyRe.MatchString(id) {
		return badRequest("a draft id is 8-64 letters, digits, _ . : -")
	}
	deleted, err := s.repo.DeleteTerminalDraft(ctx, id, s.location(), a.DeviceID)
	if err != nil {
		return err
	}
	if deleted {
		s.publish(terminal.EventDraftRemoved, map[string]any{"id": id})
	}
	return nil
}
