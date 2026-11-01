package transport

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/settingslog"
)

// Settings history (2.3.11): every settings PUT is wrapped by audited, which
// reads its section before and after; when a 2xx changed anything, the
// change is stored (settings_log, 90 days), put in the developer event log
// (kind settings) and logged with who made it — the paired tablet by name,
// or the Pi web page (a manager session).

// SetSettingsLog wires the settings history store (nil = not kept, the
// event log still gets the change).
func (s *Server) SetSettingsLog(r settingslog.Repository) { s.settingsLog = r }

// audited wraps a settings PUT handler. Audited PUTs run one at a time so
// a change is never credited to another caller.
func (s *Server) audited(section string, read func(*http.Request) any, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.auditMu.Lock()
		defer s.auditMu.Unlock()
		before := read(r)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h(sw, r)
		if sw.status < 200 || sw.status > 299 {
			return
		}
		changes := settingslog.Diff(before, read(r))
		if len(changes) == 0 {
			return
		}
		s.recordSettings(r, settingslog.Entry{At: time.Now(), Section: section, By: settingsBy(r), Changes: changes})
	}
}

func (s *Server) recordSettings(r *http.Request, e settingslog.Entry) {
	s.devLog.Add(devlog.Info, devlog.KindSettings, e.Text())
	s.logger.Info().Ctx(r.Context()).Str("section", e.Section).Str("by_kind", e.By.Kind).Str("by_name", e.By.Name).
		Str("device_id", e.By.DeviceID).Interface("changes", e.Changes).Msg("Setting changed")
	if s.settingsLog == nil {
		return
	}
	if err := s.settingsLog.Add(r.Context(), e); err != nil {
		s.logger.Warn().Ctx(r.Context()).Err(err).Str("section", e.Section).Msg("Could not save the settings history")
	}
}

// settingsBy: the paired tablet that called, else the web page's session.
func settingsBy(r *http.Request) settingslog.By {
	if d := requestDevice(r); d != nil {
		return settingslog.By{Kind: settingslog.ByTablet, Name: d.Name, DeviceID: d.ID}
	}
	return settingslog.By{Kind: settingslog.ByWeb, Name: "Pi web page"}
}

// statusWriter remembers the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// --- what each section reads as ----------------------------------------------

func (s *Server) readIntegration(part func(order.IntegrationSettings) any) func(*http.Request) any {
	return func(*http.Request) any { return part(s.service.IntegrationSettings()) }
}

func (s *Server) readDeliveries(r *http.Request) any { return s.service.DeliverySettings(r.Context()) }

func (s *Server) readAutoAccept(r *http.Request) any {
	v, err := s.service.GetAutoAccept(r.Context())
	if err != nil {
		return nil
	}
	return autoAcceptDTO{AutoAccept: v}
}

func (s *Server) readAutoUpdate(r *http.Request) any {
	if s.updates == nil {
		return nil
	}
	on, err := s.updates.AutoUpdate(r.Context())
	if err != nil {
		return nil
	}
	return map[string]bool{"auto_update": on}
}

func (s *Server) readReceipt(r *http.Request) any { return s.service.ReceiptPrefs(r.Context()) }

func (s *Server) readFloorPrefs(r *http.Request) any {
	p := s.service.FloorPrefs(r.Context())
	return map[string]any{"green_till_minutes": p.GreenTillMinutes, "tip_tax_percent": float64(p.TipTaxBps) / 100}
}

// readDriverReset: the time only (saving it also stamps last_run).
func (s *Server) readDriverReset(r *http.Request) any {
	rs, err := s.service.GetDriverReset(r.Context())
	if err != nil {
		return nil
	}
	return map[string]string{"time": rs.Time}
}

// --- GET /api/settings/changes -----------------------------------------------

const (
	settingsChangesDefault = 100
	settingsChangesMax     = 500
)

// GET /api/settings/changes?limit=N -> {changes: [newest first], keep_days}.
func (s *Server) handleSettingsChanges(w http.ResponseWriter, r *http.Request) {
	limit := settingsChangesDefault
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("limit must be a positive number", "bad_request"))
			return
		}
		limit = min(n, settingsChangesMax)
	}
	out := []settingslog.Entry{}
	if s.settingsLog != nil {
		got, err := s.settingsLog.List(r.Context(), time.Now().Add(-settingslog.Keep), limit)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, err)
			return
		}
		out = got
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"changes": out, "keep_days": int(settingslog.Keep / (24 * time.Hour))})
}
