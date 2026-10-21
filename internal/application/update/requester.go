// Package update lets a store (Pi web page), the admin website or the
// console update the Pi's agent — or roll it back — through the Pi's root
// updater. The agent never installs anything itself: it gates the request
// (only right after Close batch; a forced one waits until the store is idle),
// drops update.request next to the database, and follows update.status, which
// the root side (quicc-update.path → update.sh) writes. Every state change is
// reported to main (RPC agent.update.status) and each finished update is
// Pushovered to the developer once.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// RPC message types.
const (
	MsgUpdate   = "agent.update"        // main → agent
	MsgRollback = "agent.rollback"      // main → agent
	MsgStatus   = "agent.update.status" // agent → main report
)

// Files the root updater and the agent share (in the update dir).
const (
	DefaultDir  = "/var/lib/quicc-agent"
	RequestFile = "update.request"
	StatusFile  = "update.status"
)

// Actions.
const (
	ActionUpdate   = "update"
	ActionRollback = "rollback"
)

// States (update.status + the agent's own waiting/requested).
const (
	StateIdle       = "idle"
	StateWaiting    = "waiting_for_idle"
	StateRequested  = "requested"
	StateRunning    = "running"
	StateOK         = "ok"
	StateUpToDate   = "up_to_date"
	StateFailed     = "failed"
	StateRolledBack = "rolled_back"
)

// Refusal codes.
const (
	CodeNotAfterClose = "not_after_close"
	CodeOrdersOpen    = "orders_open"
	CodeInProgress    = "update_in_progress"
	CodeNotSupported  = "not_supported"
)

const (
	pollEvery     = 10 * time.Second
	idleEvery     = 30 * time.Second
	pickupTimeout = 10 * time.Minute // request written, root updater never started
	runningStale  = 20 * time.Minute // "running" this long = the updater died
	showTerminal  = 15 * time.Minute // a finished state stays the headline this long
	notifyMaxAge  = 24 * time.Hour   // never page about an old result
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Status is update.status (written by the root updater) — also the payload
// of the agent.update.status report (plus force).
type Status struct {
	ID          string `json:"id"`
	Action      string `json:"action"`
	RequestedBy string `json:"requested_by"`
	State       string `json:"state"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	FromSHA     string `json:"from_sha"`
	ToSHA       string `json:"to_sha"`
	Error       string `json:"error"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at"`
}

// Terminal reports whether the state is a finished one.
func Terminal(state string) bool {
	switch state {
	case StateOK, StateUpToDate, StateFailed, StateRolledBack:
		return true
	}
	return false
}

// Pending is the request this agent is carrying (API shape).
type Pending struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	Force       bool      `json:"force"`
	RequestedBy string    `json:"requested_by"`
	RequestedAt time.Time `json:"requested_at"`
}

// job is Pending plus when update.request was written (persisted in
// settings.update_job, so a forced request waiting for idle survives a
// restart and the restarted agent still knows who asked).
type job struct {
	Pending
	WrittenAt *time.Time `json:"written_at,omitempty"`
}

// Reply answers agent.update / agent.rollback and the HTTP routes.
type Reply struct {
	Accepted bool   `json:"accepted"`
	State    string `json:"state"`
	Code     string `json:"code"`
	ID       string `json:"id,omitempty"`
}

// View is GET /api/update/status.
type View struct {
	CurrentVersion  string   `json:"current_version"`
	CurrentCommit   string   `json:"current_commit"`
	Env             string   `json:"env"`
	LatestVersion   string   `json:"latest_version"`
	UpdateAvailable bool     `json:"update_available"`
	State           string   `json:"state"`
	Pending         *Pending `json:"pending"`
	Last            *Status  `json:"last"`
	CanUpdateNow    bool     `json:"can_update_now"`
	BlockedReason   string   `json:"blocked_reason"`
}

// Health is the agent.health `update` block.
type Health struct {
	CurrentVersion  string  `json:"current_version"`
	LatestVersion   string  `json:"latest_version"`
	UpdateAvailable bool    `json:"update_available"`
	State           string  `json:"state"`
	Last            *Status `json:"last"`
}

// OrderCounts reports the open batch's orders: all of them, and the ones
// still in play (arrived / accepted / ready / on_delivery).
type OrderCounts func(ctx context.Context) (total, active int64, err error)

// Store persists the job + the Pushover dedupe key (settings row).
type Store interface {
	GetUpdateState(ctx context.Context) (job, notified string, err error)
	SetUpdateJob(ctx context.Context, job string) error
	SetUpdateNotified(ctx context.Context, key string) error
}

// Reporter sends an RPC report to main (rpc.Registry.Report).
type Reporter func(ctx context.Context, msgType string, payload any) error

// Notifier pages the developer (notify.Notifier.Send — never the boss).
type Notifier func(ctx context.Context, message, sound string) error

// Config is the requester's environment.
type Config struct {
	Dir       string // where update.request / update.status live
	Supported bool   // this machine has the root updater
	Version   string
	Commit    string
	Env       string
	StoreName func() string
	Now       func() time.Time
}

// Requester gates, writes and follows update requests.
type Requester struct {
	cfg    Config
	orders OrderCounts
	store  Store
	report Reporter
	notify Notifier
	latest *Latest
	logger zerolog.Logger
	wake   chan struct{}

	mu         sync.Mutex
	job        *job
	file       *Status // newest update.status read
	notified   string  // "<id>:<state>" already paged
	reported   string  // "<id>:<state>" main already has (this process)
	lastIdleAt time.Time
}

// Supported decides whether this machine can update itself: a Pi
// (APP_ENV prod|staging, data dir /var/lib/quicc-agent, the root updater's
// path unit installed) — or AGENT_UPDATE_ENABLED=1 (tests).
func Supported(appEnv, dir string, enabledOverride bool) bool {
	if enabledOverride {
		return true
	}
	if ManifestEnv(appEnv) == "" || filepath.Clean(dir) != DefaultDir {
		return false
	}
	for _, d := range []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"} {
		if _, err := os.Stat(filepath.Join(d, "quicc-update.path")); err == nil {
			return true
		}
	}
	return false
}

// ManifestEnv maps APP_ENV to the manifest folder ("" = no updates).
func ManifestEnv(appEnv string) string {
	switch strings.ToLower(appEnv) {
	case "prod", "production", "prd":
		return "prod"
	case "staging", "stg":
		return "staging"
	}
	return ""
}

func New(cfg Config, orders OrderCounts, store Store, report Reporter, notify Notifier, latest *Latest, logger zerolog.Logger) *Requester {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.StoreName == nil {
		cfg.StoreName = func() string { return "" }
	}
	return &Requester{cfg: cfg, orders: orders, store: store, report: report, notify: notify, latest: latest,
		wake: make(chan struct{}, 1), logger: logger.With().Str("module", "update").Logger()}
}

// Load restores the persisted job + dedupe key and reads update.status once.
func (r *Requester) Load(ctx context.Context) {
	raw, notified, err := r.store.GetUpdateState(ctx)
	if err != nil {
		r.logger.Warn().Ctx(ctx).Err(err).Msg("update state not loaded")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notified = notified
	if raw != "" {
		var j job
		if json.Unmarshal([]byte(raw), &j) == nil && j.ID != "" {
			r.job = &j
		}
	}
	r.readStatusLocked()
}

// Wake asks the loop to look now.
func (r *Requester) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run follows the job: every 10 s while something is waiting / requested /
// running or main hasn't heard the latest state yet; idle check every 30 s
// for a forced request.
func (r *Requester) Run(ctx context.Context) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	r.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.wake:
		}
		r.Tick(ctx)
	}
}

// Tick is one pass of the loop (exported for tests). update.status is only
// re-read while a request is in progress (Load reads it once at startup).
func (r *Requester) Tick(ctx context.Context) {
	r.mu.Lock()
	now := r.cfg.Now()
	if r.job != nil && r.job.WrittenAt == nil && now.Sub(r.lastIdleAt) >= idleEvery {
		r.lastIdleAt = now
		r.tryWriteWaitingLocked(ctx)
	}
	if r.inProgressLocked(now) {
		r.readStatusLocked()
	}
	eff := r.effectiveLocked(now)
	done := eff == nil || (r.reported == key(eff) &&
		(!Terminal(eff.State) || r.notified == key(eff) || !r.recent(eff, notifyMaxAge)))
	r.mu.Unlock()
	if done {
		return
	}
	r.reportAndNotify(ctx, eff)
}

// Request is the single entry point for agent.update / agent.rollback and the
// Pi web buttons. It never blocks on the root updater.
func (r *Requester) Request(ctx context.Context, action string, force bool, requestedBy, id string) Reply {
	if action != ActionUpdate && action != ActionRollback {
		return Reply{Code: "bad_request"}
	}
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
		requestedBy = "admin"
	}
	r.mu.Lock()
	now := r.cfg.Now()
	r.readStatusLocked()
	// A retried request (same id) gets the current answer.
	if r.job != nil && r.job.ID == id {
		st := r.stateLocked(now)
		r.mu.Unlock()
		return Reply{Accepted: true, State: st, ID: id}
	}
	if r.inProgressLocked(now) {
		st := r.stateLocked(now)
		r.mu.Unlock()
		return Reply{State: st, Code: CodeInProgress, ID: id}
	}
	total, active, err := r.orders(ctx)
	if err != nil {
		r.mu.Unlock()
		r.logger.Warn().Ctx(ctx).Err(err).Msg("update gate: order count failed")
		return Reply{State: StateIdle, Code: CodeOrdersOpen, ID: id}
	}
	j := &job{Pending: Pending{ID: id, Action: action, Force: force && action == ActionUpdate, RequestedBy: requestedBy, RequestedAt: now.UTC()}}
	switch {
	case action == ActionRollback && active > 0:
		r.mu.Unlock()
		return Reply{State: StateIdle, Code: CodeOrdersOpen, ID: id}
	case action == ActionUpdate && !j.Force && total > 0:
		r.mu.Unlock()
		return Reply{State: StateIdle, Code: CodeNotAfterClose, ID: id}
	case j.Force && active > 0:
		r.job = j
		r.lastIdleAt = now
		r.persistJobLocked(ctx)
		r.mu.Unlock()
		r.logger.Info().Ctx(ctx).Str("id", id).Str("requested_by", requestedBy).Int64("orders_open", active).
			Msg("forced update accepted — waiting for the store to be idle")
		r.Wake()
		return Reply{Accepted: true, State: StateWaiting, ID: id}
	}
	if err := r.writeRequest(j, now); err != nil {
		r.mu.Unlock()
		r.logger.Error().Ctx(ctx).Err(err).Msg("update.request not written")
		return Reply{State: StateIdle, Code: CodeNotSupported, ID: id}
	}
	r.job = j
	r.persistJobLocked(ctx)
	r.mu.Unlock()
	r.logger.Info().Ctx(ctx).Str("id", id).Str("action", action).Str("requested_by", requestedBy).Msg("update requested")
	r.Wake()
	return Reply{Accepted: true, State: StateRequested, ID: id}
}

// View is GET /api/update/status.
func (r *Requester) View(ctx context.Context) View {
	v := View{CurrentVersion: r.cfg.Version, CurrentCommit: r.cfg.Commit, Env: r.cfg.Env}
	v.LatestVersion, v.UpdateAvailable = r.latest.Get(ctx)
	r.mu.Lock()
	now := r.cfg.Now()
	r.readStatusLocked()
	v.State = r.stateLocked(now)
	if r.job != nil && (v.State == StateWaiting || v.State == StateRequested || v.State == StateRunning) {
		p := r.job.Pending
		v.Pending = &p
	}
	v.Last = r.lastLocked(now)
	inProgress := r.inProgressLocked(now)
	r.mu.Unlock()
	switch {
	case !r.cfg.Supported:
		v.BlockedReason = CodeNotSupported
	case inProgress:
		v.BlockedReason = CodeInProgress
	default:
		total, _, err := r.orders(ctx)
		if err != nil || total > 0 {
			v.BlockedReason = CodeNotAfterClose
		}
	}
	v.CanUpdateNow = v.BlockedReason == ""
	return v
}

// Health is the agent.health `update` block.
func (r *Requester) Health(ctx context.Context) Health {
	h := Health{CurrentVersion: r.cfg.Version}
	h.LatestVersion, h.UpdateAvailable = r.latest.Get(ctx)
	r.mu.Lock()
	now := r.cfg.Now()
	h.State = r.stateLocked(now)
	h.Last = r.lastLocked(now)
	r.mu.Unlock()
	return h
}

// --- internals (r.mu held) ----------------------------------------------------

func key(s *Status) string { return s.ID + ":" + s.State }

func (r *Requester) readStatusLocked() {
	b, err := os.ReadFile(filepath.Join(r.cfg.Dir, StatusFile))
	if err != nil {
		return
	}
	var s Status
	if json.Unmarshal(b, &s) != nil || s.ID == "" || s.State == "" {
		return
	}
	r.file = &s
}

// effectiveLocked is the status that describes the newest request: the
// status file when it is about our job (or there is no job), else a status
// the agent makes up for the part before the root updater starts.
func (r *Requester) effectiveLocked(now time.Time) *Status {
	j := r.job
	if j == nil || (r.file != nil && r.file.ID == j.ID) {
		if r.file == nil {
			return nil
		}
		s := *r.file
		if s.State == StateRunning {
			if at, err := time.Parse(time.RFC3339, s.StartedAt); err == nil && now.Sub(at) > runningStale {
				s.State, s.Error = StateFailed, "the updater stopped without finishing"
				s.FinishedAt = at.Add(runningStale).UTC().Format(time.RFC3339)
			}
		}
		if s.RequestedBy == "" && j != nil {
			s.RequestedBy = j.RequestedBy
		}
		return &s
	}
	s := &Status{ID: j.ID, Action: j.Action, RequestedBy: j.RequestedBy, FromVersion: r.cfg.Version,
		FromSHA: r.latest.OwnSHA(), StartedAt: j.RequestedAt.UTC().Format(time.RFC3339)}
	switch {
	case j.WrittenAt == nil:
		s.State = StateWaiting
	case now.Sub(*j.WrittenAt) < pickupTimeout:
		s.State = StateRequested
	default:
		s.State, s.Error = StateFailed, "the Pi's updater didn't pick up the request"
		s.FinishedAt = j.WrittenAt.Add(pickupTimeout).UTC().Format(time.RFC3339)
	}
	return s
}

func (r *Requester) stateLocked(now time.Time) string {
	eff := r.effectiveLocked(now)
	if eff == nil {
		return StateIdle
	}
	if Terminal(eff.State) {
		if r.recent(eff, showTerminal) {
			return eff.State
		}
		return StateIdle
	}
	return eff.State
}

func (r *Requester) lastLocked(now time.Time) *Status {
	if eff := r.effectiveLocked(now); eff != nil && Terminal(eff.State) {
		return eff
	}
	if r.file != nil && Terminal(r.file.State) {
		s := *r.file
		return &s
	}
	return nil
}

func (r *Requester) inProgressLocked(now time.Time) bool {
	switch r.stateLocked(now) {
	case StateWaiting, StateRequested, StateRunning:
		return true
	}
	return false
}

func (r *Requester) recent(s *Status, within time.Duration) bool {
	at, err := time.Parse(time.RFC3339, s.FinishedAt)
	if err != nil {
		at, err = time.Parse(time.RFC3339, s.StartedAt)
	}
	return err == nil && r.cfg.Now().Sub(at) < within
}

func (r *Requester) tryWriteWaitingLocked(ctx context.Context) {
	_, active, err := r.orders(ctx)
	if err != nil || active > 0 {
		return
	}
	now := r.cfg.Now()
	if err := r.writeRequest(r.job, now); err != nil {
		r.logger.Error().Ctx(ctx).Err(err).Msg("update.request not written")
		return
	}
	r.persistJobLocked(ctx)
	r.logger.Info().Ctx(ctx).Str("id", r.job.ID).Msg("store idle — forced update requested")
}

// writeRequest drops update.request (tmp + rename, 0644) and stamps the job.
func (r *Requester) writeRequest(j *job, now time.Time) error {
	body, _ := json.Marshal(map[string]string{
		"id": j.ID, "action": j.Action, "requested_by": j.RequestedBy, "at": now.UTC().Format(time.RFC3339),
	})
	if err := WriteFileAtomic(filepath.Join(r.cfg.Dir, RequestFile), append(body, '\n')); err != nil {
		return err
	}
	at := now.UTC()
	j.WrittenAt = &at
	return nil
}

func (r *Requester) persistJobLocked(ctx context.Context) {
	b, _ := json.Marshal(r.job)
	if err := r.store.SetUpdateJob(ctx, string(b)); err != nil {
		r.logger.Warn().Ctx(ctx).Err(err).Msg("update job not persisted")
	}
}

// reportAndNotify tells main about a new state (retried every tick until
// main has it) and pages the developer once per finished state.
func (r *Requester) reportAndNotify(ctx context.Context, eff *Status) {
	k := key(eff)
	r.mu.Lock()
	needReport := r.reported != k
	force := r.job != nil && r.job.ID == eff.ID && r.job.Force
	needNotify := Terminal(eff.State) && r.notified != k && r.recent(eff, notifyMaxAge)
	if needNotify {
		r.notified = k // claim it before the send: one page, even if the send fails
	}
	r.mu.Unlock()

	if needReport && r.report != nil {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := r.report(rctx, MsgStatus, struct {
			Status
			Force bool `json:"force"`
		}{*eff, force})
		cancel()
		if err == nil {
			r.mu.Lock()
			r.reported = k
			r.mu.Unlock()
		} else {
			r.logger.Debug().Err(err).Msg("agent.update.status not sent; retrying")
		}
	}
	if needNotify {
		if err := r.store.SetUpdateNotified(ctx, k); err != nil {
			r.logger.Warn().Ctx(ctx).Err(err).Msg("update notify key not persisted")
		}
		msg, sound := Message(r.cfg.StoreName(), eff)
		r.logger.Info().Ctx(ctx).Str("id", eff.ID).Str("state", eff.State).Str("error", eff.Error).Msg(msg)
		if r.notify != nil {
			if err := r.notify(ctx, msg, sound); err != nil {
				r.logger.Warn().Ctx(ctx).Err(err).Msg("update Pushover failed")
			}
		}
	}
}

// Message is the developer Pushover for a finished update or rollback.
func Message(store string, s *Status) (msg, sound string) {
	who := "Pi"
	if store != "" {
		who = store + " Pi"
	}
	from, to := orQ(s.FromVersion), orQ(s.ToVersion)
	errText := ""
	if s.Error != "" {
		errText = ": " + s.Error
	}
	if s.Action == ActionRollback {
		switch s.State {
		case StateOK:
			return fmt.Sprintf("%s rolled back %s → %s", who, from, to), "classical"
		default:
			return fmt.Sprintf("%s rollback FAILED — still on %s%s", who, from, errText), "siren"
		}
	}
	switch s.State {
	case StateOK:
		return fmt.Sprintf("%s updated %s → %s", who, from, to), "classical"
	case StateUpToDate:
		return fmt.Sprintf("%s already up to date (%s)", who, from), "classical"
	case StateRolledBack:
		return fmt.Sprintf("%s update FAILED — rolled back to %s%s", who, from, errText), "siren"
	default:
		return fmt.Sprintf("%s update FAILED%s", who, errText), "siren"
	}
}

func orQ(v string) string {
	if v == "" {
		return "?"
	}
	return v
}

// WriteFileAtomic writes path via a temp file in the same directory and a
// rename, mode 0644 (the root updater reads it; a path unit watching the
// final name never sees a half-written file).
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
