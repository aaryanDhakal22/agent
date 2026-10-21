package transport

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/application/printernet"
	"quiccpos/agent/internal/application/update"
	"quiccpos/agent/internal/infra/netscan"
)

// Pi updates + printers on the network (2.0.6): GET /api/update/status,
// POST /api/update, POST /api/update/rollback, GET /api/printers/found,
// POST /api/printers/add.

const printerAddWait = 15 * time.Second

// SetUpdates wires the updater and the LAN printer watcher (either may be nil).
func (s *Server) SetUpdates(u *update.Requester, w *printernet.Watcher) {
	s.updates, s.printerNet = u, w
}

// updateRefusal is the human line for an update/rollback refusal code.
func updateRefusal(code string) string {
	switch code {
	case update.CodeNotAfterClose:
		return "Updates run right after Close batch — close the batch first."
	case update.CodeOrdersOpen:
		return "Orders are still open — finish or close them first."
	case update.CodeInProgress:
		return "An update is already in progress."
	case update.CodeNotSupported:
		return "This machine can't update itself (only an installed Pi can)."
	}
	return "The update couldn't start."
}

// GET /api/update/status
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		s.writeJSON(w, http.StatusOK, update.View{CurrentVersion: s.version, CurrentCommit: s.commit,
			State: update.StateIdle, BlockedReason: update.CodeNotSupported})
		return
	}
	s.writeJSON(w, http.StatusOK, s.updates.View(r.Context()))
}

// POST /api/update — Pi web, manager session: update now (after close only).
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	s.requestUpdate(w, r, update.ActionUpdate)
}

// POST /api/update/rollback — Pi web, manager session: back to the previous
// release (idle store only).
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	s.requestUpdate(w, r, update.ActionRollback)
}

func (s *Server) requestUpdate(w http.ResponseWriter, r *http.Request, action string) {
	if s.updates == nil {
		s.writeJSON(w, http.StatusNotImplemented, errCodeMap(updateRefusal(update.CodeNotSupported), update.CodeNotSupported))
		return
	}
	rep := s.updates.Request(r.Context(), action, false, "pi-web", "")
	switch {
	case rep.Accepted:
		s.writeJSON(w, http.StatusAccepted, map[string]string{"id": rep.ID, "state": rep.State})
	case rep.Code == update.CodeNotSupported:
		s.writeJSON(w, http.StatusNotImplemented, errCodeMap(updateRefusal(rep.Code), rep.Code))
	default:
		s.writeJSON(w, http.StatusConflict, errCodeMap(updateRefusal(rep.Code), rep.Code))
	}
}

// GET /api/printers/found[?refresh=1]
func (s *Server) handlePrintersFound(w http.ResponseWriter, r *http.Request) {
	if s.printerNet == nil {
		s.writeJSON(w, http.StatusOK, printernet.FoundView{Printers: []printernet.Found{}})
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	s.writeJSON(w, http.StatusOK, s.printerNet.FoundList(r.Context(), refresh))
}

// POST /api/printers/add {name, ip, mac} → RPC printer.add → main creates the
// printer and republishes store_config.
func (s *Server) handlePrinterAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		IP   string `json:"ip"`
		MAC  string `json:"mac"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	name := strings.TrimSpace(body.Name)
	ip := net.ParseIP(strings.TrimSpace(body.IP))
	mac := netscan.NormalizeMAC(body.MAC)
	switch {
	case name == "" || utf8.RuneCountInString(name) > 40:
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("Give the printer a name (1-40 characters).", "bad_request"))
		return
	case ip == nil || ip.To4() == nil:
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("That isn't a printer address.", "bad_request"))
		return
	case strings.TrimSpace(body.MAC) != "" && mac == "":
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("That isn't a MAC address.", "bad_request"))
		return
	}
	key, _ := s.settings.GetStoreAPIKey(r.Context())
	if key == "" || s.diag.RPC == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi isn't linked to a store.", "main_offline"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), printerAddWait)
	defer cancel()
	status, reply, err := s.diag.RPC.Send(ctx, printernet.MsgPrinterAdd, map[string]string{"name": name, "ip": ip.To4().String(), "mac": mac})
	if err != nil || status != http.StatusOK {
		// A 202 is an older main that doesn't know printer.add.
		s.logger.Warn().Ctx(r.Context()).Err(err).Int("status", status).Msg("printer.add not answered")
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("Can't reach the server — try again.", "main_offline"))
		return
	}
	var res struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
		ID   string `json:"id"`
		Name string `json:"name"`
		IP   string `json:"ip"`
	}
	_ = json.Unmarshal(reply, &res)
	switch {
	case res.OK:
		s.logger.Info().Ctx(r.Context()).Str("printer_id", res.ID).Str("name", res.Name).Str("ip", res.IP).Msg("printer added from the network list")
		s.writeJSON(w, http.StatusCreated, map[string]string{"id": res.ID, "name": res.Name, "ip": res.IP})
	case res.Code == "name_taken":
		s.writeJSON(w, http.StatusConflict, errCodeMap("A printer with that name already exists.", "name_taken"))
	default:
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("The server didn't accept that printer.", "bad_request"))
	}
}
