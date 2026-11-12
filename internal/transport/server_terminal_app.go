package transport

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// Terminal self-update (contract §2.10w): the newest terminal APK the Pi
// has cached from main, served to terminal keys only.

// GET /api/terminal/app/latest -> the cached release + install_window, or 204.
func (s *Server) handleTerminalAppLatest(w http.ResponseWriter, r *http.Request) {
	if s.termApps == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cur := s.termApps.Current()
	if cur == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	window := false
	if s.installWindow != nil {
		window = s.installWindow(r.Context())
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"app": cur.App, "version_code": cur.VersionCode, "version_name": cur.VersionName,
		"sha256": cur.SHA256, "size": cur.Size, "created_at": cur.CreatedAt, "install_window": window})
}

// GET /api/terminal/app/apk -> the cached APK (Range supported).
func (s *Server) handleTerminalAppAPK(w http.ResponseWriter, r *http.Request) {
	if s.termApps == nil || s.termApps.Current() == nil {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("no terminal release cached", "not_found"))
		return
	}
	cur := s.termApps.Current()
	f, err := os.Open(s.termApps.Path(*cur))
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("cached APK missing", "not_found"))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("X-Sha256", cur.SHA256)
	w.Header().Set("X-Version-Code", strconv.FormatInt(cur.VersionCode, 10))
	http.ServeContent(w, r, filepath.Base(f.Name()), cur.CreatedAt, f)
}
