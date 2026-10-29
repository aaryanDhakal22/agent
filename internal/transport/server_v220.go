package transport

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/device"
)

// 2.2.0: the Pi auto-update toggle, the developer-mode event feed and the
// Quibase version each paired tablet reports (X-Quibase-Version).

const (
	// quibaseVersionHeader is sent by Quibase >= 1.6.0 on every request
	// (and /api/events): "<name>+<code>".
	quibaseVersionHeader = "X-Quibase-Version"
	// appVersionEvery: an unchanged version is re-stamped at most this often.
	appVersionEvery = 10 * time.Minute
	// maxAppVersion bounds a stored version string.
	maxAppVersion = 64
)

// SetDevLog wires the developer-mode event ring (GET /api/dev/events).
func (s *Server) SetDevLog(r *devlog.Ring) { s.devLog = r }

// --- GET/PUT /api/settings/auto-update --------------------------------------

type autoUpdateDTO struct {
	AutoUpdate *bool `json:"auto_update"`
}

func (s *Server) handleGetAutoUpdate(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("updates aren't available", "not_available"))
		return
	}
	on, err := s.updates.AutoUpdate(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"auto_update": on})
}

func (s *Server) handleSetAutoUpdate(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("updates aren't available", "not_available"))
		return
	}
	var body autoUpdateDTO
	if err := decodeBody(r, &body); err != nil || body.AutoUpdate == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("auto_update (true|false) is required", "bad_request"))
		return
	}
	if err := s.updates.SetAutoUpdate(r.Context(), *body.AutoUpdate); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]bool{"auto_update": *body.AutoUpdate})
}

// --- GET /api/dev/events -----------------------------------------------------

// GET /api/dev/events?since=<seq> -> {events, last_seq}: the Pi's recent
// floor events + warn/error log lines, oldest first, <= 200 a call.
func (s *Server) handleDevEvents(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("since")), 10, 64)
	events, last := s.devLog.Since(since, devlog.MaxPage)
	s.writeJSON(w, http.StatusOK, map[string]any{"events": events, "last_seq": last})
}

// --- tablet versions ---------------------------------------------------------

// appVersionSeen is the last version recorded per device (and when).
type appVersionSeen struct {
	version string
	at      time.Time
}

// noteAppVersion records a paired device's X-Quibase-Version: written when
// it changed, or every ~10 min (last_seen-like freshness without an SD write
// per request).
func (s *Server) noteAppVersion(d *device.Device, raw string) {
	v := cleanAppVersion(raw)
	if v == "" || s.devices == nil {
		return
	}
	now := time.Now()
	s.appVerMu.Lock()
	prev, ok := s.appVersions[d.ID]
	if !ok && d.AppVersion != "" {
		prev = appVersionSeen{version: d.AppVersion}
		if d.AppVersionSeenAt != nil {
			prev.at = *d.AppVersionSeenAt
		}
	}
	changed := prev.version != v
	if !changed && now.Sub(prev.at) < appVersionEvery {
		if !ok {
			s.appVersions[d.ID] = prev
		}
		s.appVerMu.Unlock()
		return
	}
	s.appVersions[d.ID] = appVersionSeen{version: v, at: now}
	s.appVerMu.Unlock()
	if err := s.devices.SetAppVersion(context.Background(), d.ID, v, now); err != nil {
		s.logger.Warn().Err(err).Str("device_id", d.ID).Msg("save tablet app version failed")
		return
	}
	if changed && prev.version != "" {
		s.devLog.Add(devlog.Info, devlog.KindSystem, "Tablet "+d.Name+" now runs Quibase "+v+" (was "+prev.version+")")
	}
}

// cleanAppVersion keeps a printable, bounded version string ("" = none).
func cleanAppVersion(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" || len(v) > maxAppVersion || !utf8.ValidString(v) {
		return ""
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f {
			return ""
		}
	}
	return v
}

// appVersionOf is the version a device last reported ("" = never).
func (s *Server) appVersionOf(d device.Device) string {
	s.appVerMu.Lock()
	defer s.appVerMu.Unlock()
	if v, ok := s.appVersions[d.ID]; ok {
		return v.version
	}
	return d.AppVersion
}

// TabletInfo is one paired tablet in /api/diag and agent.health.
type TabletInfo struct {
	DeviceID   string     `json:"device_id"`
	Name       string     `json:"name"`
	AppVersion string     `json:"app_version"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	Connected  bool       `json:"connected"`
}

// Tablets lists the paired (not revoked) tablets with their Quibase version
// and whether they are on /api/events now.
func (s *Server) Tablets(ctx context.Context) []TabletInfo {
	out := []TabletInfo{}
	if s.devices == nil {
		return out
	}
	ds, err := s.devices.List(ctx)
	if err != nil {
		return out
	}
	s.tabletMu.Lock()
	on := make(map[string]bool, len(s.tabletStreams))
	for id := range s.tabletStreams {
		on[id] = true
	}
	s.tabletMu.Unlock()
	for _, d := range ds {
		if d.RevokedAt != nil || d.Kind != device.KindTablet {
			continue
		}
		out = append(out, TabletInfo{DeviceID: d.ID, Name: d.Name, AppVersion: s.appVersionOf(d),
			LastSeenAt: d.LastSeenAt, Connected: on[d.ID]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TabletVersions counts the tablets on /api/events by Quibase version
// ("unknown" = never sent one) — the floor.tablets.version gauge.
func (s *Server) TabletVersions() map[string]int {
	s.tabletMu.Lock()
	ids := make([]string, 0, len(s.tabletStreams))
	for id := range s.tabletStreams {
		ids = append(ids, id)
	}
	s.tabletMu.Unlock()
	out := map[string]int{}
	s.appVerMu.Lock()
	defer s.appVerMu.Unlock()
	for _, id := range ids {
		v := s.appVersions[id].version
		if v == "" {
			v = "unknown"
		}
		out[v]++
	}
	return out
}
