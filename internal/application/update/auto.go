package update

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// Auto-update right after Close batch (2.2.0): with the store's auto_update
// setting on, a successful Close batch checks this env's signed manifest and,
// when it names a different, newer agent, runs the normal update path (the
// same gate as a console / Pi-web update: the open batch must have no orders)
// with requested_by "auto". The root updater does the rest, including the
// rollback of an unhealthy new agent.

// RequestedByAuto marks an update the agent started itself after Close batch.
const RequestedByAuto = "auto"

// Auto-update outcomes (AfterClose's reason when nothing was requested).
const (
	AutoOff          = "off"
	AutoNoManifest   = "no_manifest"
	AutoUpToDate     = "up_to_date"
	AutoNotNewer     = "not_newer"
	AutoFailedBefore = "failed_before"
)

// AutoStore persists the auto_update setting (the settings row).
type AutoStore interface {
	GetAutoUpdate(ctx context.Context) (bool, error)
	SetAutoUpdate(ctx context.Context, on bool) error
}

func (r *Requester) autoStore() (AutoStore, error) {
	if a, ok := r.store.(AutoStore); ok {
		return a, nil
	}
	return nil, errors.New("auto_update is not stored here")
}

// AutoUpdate reads the auto_update setting (default true).
func (r *Requester) AutoUpdate(ctx context.Context) (bool, error) {
	a, err := r.autoStore()
	if err != nil {
		return true, err
	}
	return a.GetAutoUpdate(ctx)
}

// SetAutoUpdate saves the auto_update setting.
func (r *Requester) SetAutoUpdate(ctx context.Context, on bool) error {
	a, err := r.autoStore()
	if err != nil {
		return err
	}
	return a.SetAutoUpdate(ctx, on)
}

// AfterClose runs after a successful Close batch: with auto_update on and a
// newer agent in the manifest, it requests a normal update (requested_by
// auto). It returns the reply when it asked, else the reason it didn't.
func (r *Requester) AfterClose(ctx context.Context) (*Reply, string) {
	if on, err := r.AutoUpdate(ctx); err != nil || !on {
		return nil, AutoOff
	}
	if !r.cfg.Supported {
		return nil, CodeNotSupported
	}
	if r.latest == nil || r.latest.url == "" {
		return nil, AutoNoManifest
	}
	if err := r.latest.Refresh(ctx); err != nil {
		r.logger.Warn().Ctx(ctx).Err(err).Msg("auto-update: manifest check failed")
		return nil, AutoNoManifest
	}
	version, sha := r.latest.Current()
	own := r.latest.OwnSHA()
	switch {
	case version == "" || sha == "":
		return nil, AutoNoManifest
	case own != "" && strings.EqualFold(sha, own):
		return nil, AutoUpToDate
	case !Newer(version, r.cfg.Version):
		return nil, AutoNotNewer
	}
	// The same build already failed (or was rolled back) after an earlier
	// close: don't loop on it every night; a newer manifest is tried again.
	r.mu.Lock()
	r.readStatusLocked()
	last := r.file
	r.mu.Unlock()
	if last != nil && last.Action == ActionUpdate && (last.State == StateFailed || last.State == StateRolledBack) &&
		last.ToSHA != "" && strings.EqualFold(last.ToSHA, sha) {
		return nil, AutoFailedBefore
	}
	rep := r.Request(ctx, ActionUpdate, false, RequestedByAuto, "")
	if !rep.Accepted {
		r.logger.Info().Ctx(ctx).Str("code", rep.Code).Str("latest", version).Msg("auto-update after close not started")
		return &rep, rep.Code
	}
	r.logger.Info().Ctx(ctx).Str("id", rep.ID).Str("from", r.cfg.Version).Str("to", version).Msg("auto-update after close requested")
	return &rep, ""
}

// Newer reports whether the manifest's agent version should replace the
// running one: when both are plain semver (X.Y.Z, optional leading v), only
// a strictly higher one; otherwise (a rolling staging build named
// staging-latest-<sha12>, or a dev build) any different build counts — the
// caller has already checked that the binaries differ.
func Newer(manifest, running string) bool {
	m, okM := semver(manifest)
	c, okC := semver(running)
	if !okM || !okC {
		return manifest != ""
	}
	for i := 0; i < 3; i++ {
		if m[i] != c[i] {
			return m[i] > c[i]
		}
	}
	return false
}

func semver(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
