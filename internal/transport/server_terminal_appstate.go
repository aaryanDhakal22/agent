package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"quiccpos/agent/internal/application/appupdate"
	"quiccpos/agent/internal/domain/device"
)

// TerminalAppState is what a terminal reports about its app and its update
// (contract 2.10w "From home", 2.5.3): POST /api/terminal/app/state, at start,
// on every change and every 5 minutes. The Pi keeps the last one per terminal
// (memory) and hands it to main in agent.health's terminals[].update.
type TerminalAppState struct {
	VersionName string `json:"version_name"`
	VersionCode int64  `json:"version_code"`
	// State: up_to_date | downloading | cached | waiting_for_idle |
	// waiting_for_tap | installing | failed.
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// TargetCode: the release it is getting / installing (0 = none).
	TargetCode int64     `json:"target_code,omitempty"`
	At         time.Time `json:"at"`
	Android    string    `json:"android,omitempty"`
	// Permissions: what the update needs, e.g. {"install_packages": true}.
	Permissions map[string]bool `json:"permissions,omitempty"`
	// ReceivedAt: when the Pi got it (the terminal's last contact for this).
	ReceivedAt time.Time `json:"received_at"`
}

var terminalAppStates = map[string]bool{"up_to_date": true, "downloading": true, "cached": true, "waiting_for_idle": true,
	"waiting_for_tap": true, "installing": true, "failed": true}

func (s *Server) terminalAppState(id string) (TerminalAppState, bool) {
	s.tabletMu.Lock()
	defer s.tabletMu.Unlock()
	st, ok := s.appStates[id]
	return st, ok
}

// POST /api/terminal/app/state (terminal key) → 204.
func (s *Server) handleTerminalAppState(w http.ResponseWriter, r *http.Request) {
	d, ok := r.Context().Value(deviceCtxKey{}).(*device.Device)
	if !ok || d.Kind != device.KindTerminal {
		s.writeJSON(w, http.StatusForbidden, errCodeMap("a terminal reports its own app state", "terminal_required"))
		return
	}
	var in TerminalAppState
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("bad body", "bad_request"))
		return
	}
	in.State = strings.TrimSpace(in.State)
	if !terminalAppStates[in.State] {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("state is up_to_date, downloading, cached, waiting_for_idle, waiting_for_tap, installing or failed", "bad_request"))
		return
	}
	in.Reason = clipRunes(strings.TrimSpace(in.Reason), 200)
	in.VersionName = clipRunes(strings.TrimSpace(in.VersionName), 40)
	in.Android = clipRunes(strings.TrimSpace(in.Android), 40)
	if len(in.Permissions) > 10 {
		in.Permissions = nil
	}
	in.ReceivedAt = time.Now().UTC()
	if in.At.IsZero() {
		in.At = in.ReceivedAt
	}
	s.tabletMu.Lock()
	if s.appStates == nil {
		s.appStates = map[string]TerminalAppState{}
	}
	prev, had := s.appStates[d.ID]
	s.appStates[d.ID] = in
	hook := s.onAppState
	s.tabletMu.Unlock()
	if hook != nil && (!had || prev.State != in.State || prev.VersionCode != in.VersionCode) {
		hook() // a new state: main hears it with the next heartbeat, now
	}
	w.WriteHeader(http.StatusNoContent)
}

// OnTerminalAppState runs after a terminal reports a new update state (the
// heartbeat's Wake: main's Store › Devices sees it at once).
func (s *Server) OnTerminalAppState(fn func()) {
	s.tabletMu.Lock()
	s.onAppState = fn
	s.tabletMu.Unlock()
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// UpdateNowResult is the terminals.update_now reply (contract 2.10w "From
// home"): ok = a release is cached and the terminals were told; code
// no_release = main has none for the terminal app (or the Pi couldn't get it).
type UpdateNowResult struct {
	OK         bool                `json:"ok"`
	Code       string              `json:"code,omitempty"`
	CheckError string              `json:"check_error,omitempty"`
	Release    *appupdate.Release  `json:"release"`
	Terminals  []UpdateNowTerminal `json:"terminals"`
}

// UpdateNowTerminal is one terminal in the reply: told = it was on
// /api/events when the Pi sent terminal.app.install (a terminal that isn't
// connected hears it at its next start / announcement).
type UpdateNowTerminal struct {
	DeviceID   string            `json:"device_id"`
	Name       string            `json:"name"`
	AppVersion string            `json:"app_version"`
	Connected  bool              `json:"connected"`
	Told       bool              `json:"told"`
	UpToDate   bool              `json:"up_to_date"`
	Update     *TerminalAppState `json:"update,omitempty"`
}

// TerminalsUpdateNow handles main's terminals.update_now (2.5.3, run off the
// SSE reader): it first makes sure the newest terminal release is cached
// (check = the terminal app updater's Check), then announces it with
// terminal.app.install {reason: "update_now"} — each terminal installs it at
// its next quiet moment — and answers per terminal.
func (s *Server) TerminalsUpdateNow(check func(context.Context) error, current func() *appupdate.Release,
	announce func(appupdate.Release)) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, _ json.RawMessage) (any, error) {
		res := UpdateNowResult{Terminals: []UpdateNowTerminal{}}
		cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		if err := check(cctx); err != nil {
			res.CheckError = err.Error()
		}
		cancel()
		rel := current()
		res.Release = rel
		for _, t := range s.Terminals(ctx) {
			u := UpdateNowTerminal{DeviceID: t.DeviceID, Name: t.Name, AppVersion: t.AppVersion, Connected: t.Connected, Update: t.Update}
			if rel != nil && t.Update != nil && t.Update.VersionCode >= rel.VersionCode {
				u.UpToDate = true
			}
			u.Told = rel != nil && t.Connected && !u.UpToDate
			res.Terminals = append(res.Terminals, u)
		}
		if rel == nil {
			res.Code = "no_release"
			return res, nil
		}
		announce(*rel)
		res.OK = true
		s.logger.Info().Ctx(ctx).Int64("version_code", rel.VersionCode).Int("terminals", len(res.Terminals)).
			Msg("Terminals told to update now (from main)")
		return res, nil
	}
}
