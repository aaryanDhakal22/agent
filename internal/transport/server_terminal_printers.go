package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/device"
)

// The Pi's copy of each counter terminal's receipt printer settings
// (contract §2.10ap, the owner 2026-11-14: a terminal that had lost its
// printer settings printed its slips at 48 columns on a 42-column printer).
// A terminal sends its profile whenever setup saves it and reads the copies
// back when it has none for the printer it finds. Device-level: a terminal's
// key alone (no staff login: a fresh install has nobody logged in yet).

// maxTerminalPrinterBody bounds a PUT body (a profile + a route + the key).
const maxTerminalPrinterBody = 2*device.MaxTerminalPrinterJSON + 1024

func (s *Server) terminalPrinters() device.TerminalPrinterStore {
	st, _ := s.setups.(device.TerminalPrinterStore)
	return st
}

// GET /api/terminal/printer-profiles -> {profiles: [{printer_key, device_id,
// device_name, profile, route?, updated_at}]} (newest first).
func (s *Server) handleTerminalPrinterProfiles(w http.ResponseWriter, r *http.Request) {
	st := s.terminalPrinters()
	if st == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("printer copies aren't available", "not_available"))
		return
	}
	list, err := st.TerminalPrinters(r.Context())
	if err != nil {
		s.logger.Error().Ctx(r.Context()).Err(err).Msg("terminal printer copies: list failed")
		s.writeJSON(w, http.StatusInternalServerError, errCodeMap("couldn't read the printer copies", "internal"))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"profiles": list})
}

// PUT /api/terminal/printer-profile {printer_key, profile: {...}, route?:
// {reroute?, backup?}} -> 204. The calling terminal's id and name are the
// Pi's (never the body's).
func (s *Server) handleTerminalPutPrinterProfile(w http.ResponseWriter, r *http.Request) {
	st := s.terminalPrinters()
	if st == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("printer copies aren't available", "not_available"))
		return
	}
	var body struct {
		PrinterKey string          `json:"printer_key"`
		Profile    json.RawMessage `json:"profile"`
		Route      json.RawMessage `json:"route"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTerminalPrinterBody)).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.writeJSON(w, http.StatusRequestEntityTooLarge, errCodeMap("The printer settings are too large.", "too_large"))
			return
		}
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	a := actor(r)
	p := device.TerminalPrinter{PrinterKey: strings.TrimSpace(body.PrinterKey), DeviceID: a.DeviceID, DeviceName: a.DeviceName,
		Profile: body.Profile, Route: body.Route, UpdatedAt: time.Now().UTC()}
	if err := device.CheckTerminalPrinter(p); err != nil {
		msg := err.Error()
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		s.writeJSON(w, http.StatusBadRequest, errCodeMap(msg, "bad_request"))
		return
	}
	if err := st.SetTerminalPrinter(r.Context(), p); err != nil {
		s.logger.Error().Ctx(r.Context()).Err(err).Str("device_id", a.DeviceID).Msg("terminal printer copy: save failed")
		s.writeJSON(w, http.StatusInternalServerError, errCodeMap("couldn't keep the printer settings", "internal"))
		return
	}
	s.logger.Info().Ctx(r.Context()).Str("device_id", a.DeviceID).Str("printer_key", p.PrinterKey).Msg("Terminal printer settings kept on the Pi")
	w.WriteHeader(http.StatusNoContent)
}
