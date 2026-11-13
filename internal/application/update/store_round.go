package update

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// The store update round (contract §2.10ad, application/storeupdate) waits
// for the store-wide quiet moment itself, then hands the Pi's own update to
// the root updater through RequestNow — no order gate here (the round's gate
// is stricter about what is in flight and looser about orders that are only
// cooking or out: they survive the restart in SQLite).

// RequestedByRound prefixes requested_by for a round's update.
const RequestedByRound = "store-update"

// RequestNow writes update.request at once (the store round decided the
// moment). A request already handed to the updater answers update_in_progress.
func (r *Requester) RequestNow(ctx context.Context, id, requestedBy string) Reply {
	if !r.cfg.Supported {
		return Reply{State: StateIdle, Code: CodeNotSupported}
	}
	if id == "" {
		id = uuid.NewString()
	}
	if !idRe.MatchString(id) {
		return Reply{Code: "bad_request"}
	}
	if requestedBy == "" {
		requestedBy = RequestedByRound
	}
	r.mu.Lock()
	now := r.cfg.Now()
	r.refreshLocked(ctx)
	if r.job != nil && r.job.ID == id || r.last != nil && r.last.ID == id || r.file != nil && r.file.ID == id {
		st := r.stateLocked(now)
		r.mu.Unlock()
		return Reply{Accepted: true, State: st, ID: id}
	}
	if st := r.stateLocked(now); st == StateRequested || st == StateRunning {
		r.mu.Unlock()
		return Reply{State: st, Code: CodeInProgress, ID: id}
	}
	j := &job{Pending: Pending{ID: id, Action: ActionUpdate, RequestedBy: requestedBy, RequestedAt: now.UTC()}}
	if err := r.writeRequest(j, now); err != nil {
		r.mu.Unlock()
		r.logger.Error().Ctx(ctx).Err(err).Msg("update.request not written")
		return Reply{State: StateIdle, Code: CodeNotSupported, ID: id}
	}
	r.job = j
	r.persistJobLocked(ctx)
	r.mu.Unlock()
	r.logger.Info().Ctx(ctx).Str("id", id).Str("requested_by", requestedBy).Msg("update requested by the store round")
	r.event("info", fmt.Sprintf("update requested by %s (%s)", requestedBy, id))
	r.Wake()
	return Reply{Accepted: true, State: StateRequested, ID: id}
}

// ResultOf is the state of request id as the requester knows it ("" = it
// doesn't): requested | running | ok | up_to_date | failed | rolled_back.
func (r *Requester) ResultOf(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked(ctx)
	now := r.cfg.Now()
	if eff := r.effectiveLocked(now); eff != nil && eff.ID == id {
		return eff.State
	}
	if r.last != nil && r.last.ID == id {
		return r.last.State
	}
	if r.file != nil && r.file.ID == id {
		return r.file.State
	}
	return ""
}

// InProgress: an update or rollback is waiting, requested or running.
func (r *Requester) InProgress() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inProgressLocked(r.cfg.Now())
}

// Supported: this machine has the root updater.
func (r *Requester) Supported() bool { return r.cfg.Supported }

// Manifest is the cached manifest (header + files; refresh = fetch now).
func (r *Requester) Manifest(ctx context.Context, refresh bool) (Manifest, bool) {
	if r.latest == nil || r.latest.url == "" {
		return Manifest{}, false
	}
	if refresh {
		if err := r.latest.Refresh(ctx); err != nil {
			r.logger.Warn().Ctx(ctx).Err(err).Msg("store round: manifest check failed")
		}
	} else {
		r.latest.Get(ctx) // starts a background refresh when stale
	}
	return r.latest.Manifest()
}

// OwnSHA is the running binary's sha256 ("" when unknown).
func (r *Requester) OwnSHA() string { return r.latest.OwnSHA() }
