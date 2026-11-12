package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/infra/printer/receipt"
)

// The width test (contract/agent.md §2.3c "Paper width"): the Pi's Printers page (or the terminal's setup)
// prints a letter ruler on ONE printer (never failing over), staff read the last letter on its first line,
// and the Pi saves that printer's print width in main (RPC printer.width); main sends the store_config back
// and every slip on that printer is laid out for it.

// MsgPrinterWidth is the agent → main RPC that saves a printer's print width.
const MsgPrinterWidth = "printer.width"

// POST /api/printers/width-test {printer_id} → prints the ruler on that printer → {results: [...]}.
func (s *Server) handlePrinterWidthTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PrinterID string `json:"printer_id"`
	}
	if err := decodeBody(r, &req); err != nil || strings.TrimSpace(req.PrinterID) == "" {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("Pick the printer to test.", "bad_request"))
		return
	}
	if s.diag.Dispatcher == nil || s.diag.WidthTest == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("printing isn't available", "not_available"))
		return
	}
	if !s.testMu.TryLock() {
		s.writeJSON(w, http.StatusConflict, errCodeMap("A printer test is already running — wait for it.", "test_in_progress"))
		return
	}
	defer s.testMu.Unlock()
	now := time.Now()
	results := s.diag.Dispatcher.PrintTest(r.Context(), strings.TrimSpace(req.PrinterID), func(e printerApp.RosterEntry) []byte {
		return s.diag.WidthTest(e.Name, now)
	})
	if results == nil {
		results = []printerApp.TestResult{}
	}
	if len(results) == 1 && results[0].ErrorCode == printerApp.TestNotConfigured && results[0].Name == "" {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("No printer with that id.", printerApp.TestNotConfigured))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// widthOfColumns is the print width saved for a ruler read-off: the widest of 48 / 42 / 32 that the
// printer fits (a printer that took 45 characters is laid out for 42); false below 32 (not a receipt
// printer).
func widthOfColumns(cols int) (int, bool) {
	if cols < receipt.Cols58 {
		return 0, false
	}
	return receipt.NormalizeCols(cols), true
}

// POST /api/printers/width {printer_id, letter} (the last letter on the ruler's first line; or
// {printer_id, print_columns: 48|42|32} to set it straight) → RPC printer.width → main saves it and sends
// the store_config back → {printer_id, name, columns_read, print_columns}.
func (s *Server) handlePrinterWidth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PrinterID    string `json:"printer_id"`
		Letter       string `json:"letter"`
		PrintColumns int    `json:"print_columns"`
	}
	if err := decodeBody(r, &req); err != nil || strings.TrimSpace(req.PrinterID) == "" {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("Pick the printer.", "bad_request"))
		return
	}
	id := strings.TrimSpace(req.PrinterID)
	read := req.PrintColumns
	if strings.TrimSpace(req.Letter) != "" {
		c, ok := receipt.ColumnsOfSymbol(req.Letter)
		if !ok {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("That letter isn't on the ruler. Pick the last letter (or number) on the first line.", "bad_letter"))
			return
		}
		read = c
	} else if read != receipt.Cols80 && read != receipt.Cols72 && read != receipt.Cols58 {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("The width is 48, 42 or 32 characters.", "bad_request"))
		return
	}
	cols, ok := widthOfColumns(read)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("Only "+itoa(read)+" characters fit: this printer is narrower than 58 mm paper and can't print receipts.", "too_narrow"))
		return
	}
	name := ""
	if s.diag.Dispatcher != nil {
		for _, e := range s.diag.Dispatcher.Roster() {
			if e.ID == id {
				name = e.Name
			}
		}
	}
	if name == "" {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("No printer with that id.", printerApp.TestNotConfigured))
		return
	}
	key, _ := s.settings.GetStoreAPIKey(r.Context())
	if key == "" || s.diag.RPC == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi isn't linked to a store.", "main_offline"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), printerAddWait)
	defer cancel()
	status, reply, err := s.diag.RPC.Send(ctx, MsgPrinterWidth, map[string]any{"printer_id": id, "print_columns": cols})
	if err != nil || status != http.StatusOK {
		// A 202 is an older main that doesn't know printer.width.
		s.logger.Warn().Ctx(r.Context()).Err(err).Int("status", status).Msg("printer.width not answered")
		if err == nil && status == http.StatusAccepted {
			s.writeJSON(w, http.StatusNotImplemented, errCodeMap("The server is too old to save printer widths — set it in Store › Printers once it is updated.", "main_unsupported"))
			return
		}
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("Can't reach the server — try again.", "main_offline"))
		return
	}
	var res struct {
		OK           bool   `json:"ok"`
		Code         string `json:"code"`
		PrintColumns int    `json:"print_columns"`
	}
	_ = json.Unmarshal(reply, &res)
	switch {
	case res.OK:
		s.logger.Info().Ctx(r.Context()).Str("printer_id", id).Str("printer", name).Int("columns_read", read).
			Int("print_columns", cols).Msg("printer width saved from the width test")
		s.writeJSON(w, http.StatusOK, map[string]any{"printer_id": id, "name": name, "columns_read": read, "print_columns": cols})
	case res.Code == "not_found":
		s.writeJSON(w, http.StatusNotFound, errCodeMap("The server doesn't know that printer.", "not_found"))
	default:
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("The server didn't accept that width.", "bad_request"))
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
