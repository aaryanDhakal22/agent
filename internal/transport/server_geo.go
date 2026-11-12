package transport

import "net/http"

// Ambiguous addresses (device routes): staff pick one of main's candidates
// or type an address to search for; main answers later with order.geo.

// POST /api/orders/{id}/geo/choose {index} -> 202 {} (main answers with order.geo).
func (s *Server) handleGeoChoose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Index *int `json:"index"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if req.Index == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("index is required", "bad_request"))
		return
	}
	if err := s.service.ChooseCandidate(r.Context(), r.PathValue("id"), *req.Index); err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]any{})
}

// POST /api/orders/{id}/geo/search {query} -> 202 {} (main answers with
// order.geo: check with places near the store, or not_found with a note).
func (s *Server) handleGeoSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if err := s.service.SearchAddress(r.Context(), r.PathValue("id"), req.Query); err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]any{})
}
