package transport

import (
	"net/http"
	"time"
)

// GET /api/terminal/status — what a POS terminal shows as alerts: every
// printer's last probe (the same entries as GET /api/printers), the Pi's
// link to main, and the menu / config snapshots in use (contract §2.10e).
func (s *Server) handleTerminalStatus(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	key, _ := s.settings.GetStoreAPIKey(r.Context())
	main := map[string]any{"state": s.sseState(key != ""), "connected": false}
	if s.sseController != nil {
		st := s.sseController.Status()
		main["connected"] = st.Connected
		main["connected_since"] = unixPtr(st.ConnectedSince)
		main["last_event_at"] = unixPtr(st.LastEventAt)
		if st.LastError != "" {
			main["last_error"] = st.LastError
		}
	}
	sess := s.terminalSvc.Session(actor(r))
	printers := s.printers.All()
	down := 0
	for _, p := range printers {
		if !p.Up {
			down++
		}
	}
	// F-15: the SD card's state, so the terminals can show a banner when the
	// Pi can't save ({trouble: "" | readonly | full | ioerr, since?}).
	db := map[string]any{"ok": true, "trouble": ""}
	if s.dbHealth != nil {
		if st := s.dbHealth(); st.Trouble != "" {
			db = map[string]any{"ok": false, "trouble": st.Trouble, "since": st.Since}
		}
	}
	out := map[string]any{
		"db":              db,
		"printers":        printers,
		"printers_down":   down,
		"main":            main,
		"menu_version":    sess.MenuVersion,
		"menu_fetched_at": sess.MenuFetchedAt,
		"config_version":  sess.ConfigVersion,
		"at":              time.Now().UTC(),
	}
	// Square (contract 2.10r §5): what the Pi last heard from main, never the token.
	if sq := s.squareStatus(); sq != nil {
		out["square"] = sq
	}
	s.writeJSON(w, http.StatusOK, out)
}
