package terminal

import "quiccpos/agent/internal/application/appupdate"

// Terminal self-update (contract/agent.md §2.10w): the Pi caches the newest
// terminal APK from main (appupdate, app "terminal") and tells the
// terminals when one is cached and when it is a good moment to install it.
const (
	// EventAppAvailable: a newer terminal APK is cached on the Pi (download
	// it now; install at the next install moment).
	EventAppAvailable = "terminal.app.available"
	// EventAppInstall: Close batch finished (its reports printed): a good
	// moment to install the cached APK if it is newer than the terminal's.
	EventAppInstall = "terminal.app.install"
)

// AnnounceApp tells the terminals about a cached release (event = one of
// the two above; reason: cached | batch_closed).
func (s *Service) AnnounceApp(event string, rel appupdate.Release, reason string) {
	s.publish(event, map[string]any{"app": rel.App, "version_code": rel.VersionCode, "version_name": rel.VersionName,
		"sha256": rel.SHA256, "size": rel.Size, "reason": reason})
}
