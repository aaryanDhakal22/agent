// Package storeupdate runs a store update round (contract §2.10ad): the Pi,
// every Quibase tablet and every Centurion terminal of a store move to a new
// release together, at one store-wide quiet moment. The Pi decides; the
// devices follow GET /api/store-update and the SSE `store.update`.
//
// Phases: idle → waiting (APKs cached? the Pi quiet?) → preparing (every
// connected device on the protocol says it is quiet) → frozen (devices show
// Updating) → pi_installing (only when the round has an agent; the root
// updater swaps the binary) → devices_installing (the round's APKs become the
// store's release; devices behind it install) → done | cancelled.
//
// The Pi is updated first and the devices only once the new Pi runs: a Pi
// update that fails cancels the round before any device installed. A device
// that fails to install never rolls the Pi back.
//
// State lives in a JSON file next to the database (no migration), so the new
// agent picks the round up after the restart its own update causes.
package storeupdate

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/safego"

	"github.com/rs/zerolog"
)

// Event is the SSE event every listener gets on a phase change.
const Event = "store.update"

// Phases.
const (
	PhaseIdle              = "idle"
	PhaseWaiting           = "waiting"
	PhasePreparing         = "preparing"
	PhaseFrozen            = "frozen"
	PhasePiInstalling      = "pi_installing"
	PhaseDevicesInstalling = "devices_installing"
	PhaseDone              = "done"
	PhaseCancelled         = "cancelled"
)

// Device kinds (= device.KindTablet / device.KindTerminal).
const (
	KindTablet   = "tablet"
	KindTerminal = "terminal"
)

// Apps.
const (
	AppQuibase  = "quibase"
	AppTerminal = "terminal"
)

// Protocol versions: the first app versions that take part in a round.
// Older devices keep their own after-Close-batch install.
const (
	ProtocolQuibase  = "2.1.1"
	ProtocolTerminal = "1.0.5"
)

// Timing (contract §2.10ad).
const (
	TickEvery      = 30 * time.Second
	PrepareWindow  = 3 * time.Minute
	BusyRetry      = 5 * time.Minute
	AppsCheckEvery = 5 * time.Minute
	InstallWindow  = 10 * time.Minute
	PiTimeout      = 30 * time.Minute
	IdleSeconds    = 60
	// FinishedShown: a done / cancelled round is shown this long, then idle.
	FinishedShown = 2 * time.Minute
	// DefaultSettle: after the agent starts, no round starts by itself for
	// this long (AGENT_STORE_ROUND_SETTLE): the tablets and terminals
	// reconnect first, else a round would see nobody to ask and update the
	// Pi under a device that is mid-order. A nudge doesn't wait.
	DefaultSettle = 2 * time.Minute
)

//go:embed compat.json
var compatJSON []byte

// Compat is this agent's compatibility floor: the oldest app versions it
// speaks with. Devices below it are locked (426 app_update_required).
type Compat struct {
	MinQuibase  string `json:"min_quibase"`
	MinTerminal string `json:"min_terminal"`
}

// Floors reads the embedded compat.json.
func Floors() Compat {
	var c Compat
	_ = json.Unmarshal(compatJSON, &c)
	return c
}

// Min is the floor for a device kind ("" = none).
func (c Compat) Min(kind string) string {
	switch kind {
	case KindTablet:
		return c.MinQuibase
	case KindTerminal:
		return c.MinTerminal
	}
	return ""
}

// Release is an app version (main's / the Pi's cache shape, trimmed).
type Release struct {
	VersionCode int64  `json:"version_code"`
	VersionName string `json:"version_name"`
}

// AgentMove is the round's Pi update.
type AgentMove struct {
	From    string `json:"from"`
	To      string `json:"to"`
	FromSHA string `json:"-"`
	ToSHA   string `json:"-"`
}

// Device is a paired tablet or terminal as the coordinator sees it.
type Device struct {
	ID        string
	Name      string
	Kind      string
	Version   string // "2.1.1+1762990000" ("" = never sent one)
	Connected bool
}

// Manifest is what the env's manifest says (contract §2.10ad headers).
type Manifest struct {
	AgentVersion string
	AgentSHA     string
	MinQuibase   string
	MinTerminal  string
	Quibase      string // quibase_version ("" = not named)
	Terminal     string // terminal_version
	// RollbackFrom (rollback_from): this manifest is a rollback (`--do ship rollback prd`): an agent running
	// exactly this version installs the manifest's OLDER agent. Any other running version treats it as usual
	// (never older), so a replayed rollback manifest can't take a Pi on another release backwards.
	RollbackFrom string
}

// RollbackFor: the manifest takes an agent running `running` back to its older agent_version.
func (m Manifest) RollbackFor(running string) bool {
	if m.RollbackFrom == "" || m.AgentVersion == "" {
		return false
	}
	x, ok1 := semver(m.RollbackFrom)
	y, ok2 := semver(running)
	return ok1 && ok2 && x == y && less(m.AgentVersion, running)
}

// participant is a device the round waits for (preparing) or installs
// (devices_installing).
type participant struct {
	Name   string    `json:"name"`
	Kind   string    `json:"kind"`
	Ready  *bool     `json:"ready,omitempty"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// storeRelease is the app versions the store is on.
type storeRelease struct {
	Quibase  *Release `json:"quibase"`
	Terminal *Release `json:"terminal"`
}

// state is the persisted round.
type state struct {
	ID           string                  `json:"id"`
	Phase        string                  `json:"phase"`
	Reason       string                  `json:"reason"`
	Since        time.Time               `json:"since"`
	Deadline     *time.Time              `json:"deadline,omitempty"`
	RequestedBy  string                  `json:"requested_by"`
	Agent        *agentState             `json:"agent,omitempty"`
	Quibase      *Release                `json:"quibase,omitempty"`
	Terminal     *Release                `json:"terminal,omitempty"`
	StoreRelease storeRelease            `json:"store_release"`
	Participants map[string]*participant `json:"participants,omitempty"`
	Stragglers   []string                `json:"stragglers,omitempty"`
	RetryAfter   time.Time               `json:"retry_after"`
	PiRequestID  string                  `json:"pi_request_id,omitempty"`
	// FailedAgentSHA: an agent build whose round failed; not tried again
	// until the manifest names another one.
	FailedAgentSHA string `json:"failed_agent_sha,omitempty"`
}

type agentState struct {
	From    string `json:"from"`
	To      string `json:"to"`
	FromSHA string `json:"from_sha"`
	ToSHA   string `json:"to_sha"`
}

// View is GET /api/store-update and the SSE body (Me nil there).
type View struct {
	ID           string       `json:"id"`
	Phase        string       `json:"phase"`
	Reason       string       `json:"reason"`
	Since        *time.Time   `json:"since"`
	Deadline     *time.Time   `json:"deadline"`
	Agent        *AgentMove   `json:"agent"`
	Quibase      *Release     `json:"quibase"`
	Terminal     *Release     `json:"terminal"`
	IdleSeconds  int          `json:"idle_seconds"`
	StoreRelease storeRelease `json:"store_release"`
	Min          MinView      `json:"min"`
	Stragglers   []string     `json:"stragglers"`
	Me           *Me          `json:"me,omitempty"`
}

// MinView is the compatibility floor on the wire.
type MinView struct {
	Quibase  string `json:"quibase"`
	Terminal string `json:"terminal"`
}

// Me is the asking device's view.
type Me struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
	InRound bool   `json:"in_round"`
	Ready   *bool  `json:"ready"`
	Behind  bool   `json:"behind"`
	Locked  bool   `json:"locked"`
}

// Reply answers a nudge (shaped like the update requester's).
type Reply struct {
	Accepted bool   `json:"accepted"`
	State    string `json:"state"`
	Code     string `json:"code"`
	ID       string `json:"id,omitempty"`
}

// StatePath is the round's file next to the database, named after it
// (agent.db → agent.store-update.json; the dev roster's agents share a
// directory).
func StatePath(dbPath string) string {
	return strings.TrimSuffix(dbPath, filepath.Ext(dbPath)) + ".store-update.json"
}

// ErrNotPreparing: a ready answer for another round / phase (409).
var ErrNotPreparing = errors.New("not_preparing")

// ErrNotInRound: a ready answer from a device the round doesn't wait for.
var ErrNotInRound = errors.New("not_in_round")

// Deps wire the coordinator to the rest of the agent (all optional but Path).
type Deps struct {
	Path string // the JSON state file
	// Settle: no automatic round this long after start (0 = DefaultSettle,
	// negative = none).
	Settle  time.Duration
	Version string // the running agent's version
	OwnSHA  func() string
	Compat  Compat
	Now     func() time.Time
	Logger  zerolog.Logger

	// Devices lists the paired tablets and terminals.
	Devices func(ctx context.Context) []Device
	// Manifest reads the env's manifest (refresh = fetch now).
	Manifest func(ctx context.Context, refresh bool) (Manifest, bool)
	// Cached is the APK the Pi has cached for an app (nil = none).
	Cached func(app string) *Release
	// CheckApps asks main for the newest APKs now (both apps).
	CheckApps func(ctx context.Context)
	// AutoOn: the store's auto_update setting.
	AutoOn func(ctx context.Context) bool
	// Busy says why the Pi isn't quiet ("" = quiet).
	Busy func(ctx context.Context) string
	// PiUpdate hands the Pi's own update to the root updater now (the round
	// already waited for the quiet moment).
	PiUpdate func(ctx context.Context, id, requestedBy string) (accepted bool, code string)
	// PiSupported: this machine can update itself (a dev agent can't: its
	// rounds carry apps only).
	PiSupported func() bool
	// PiResult is the root updater's last result for id ("" = none yet):
	// ok | up_to_date | failed | rolled_back | running | requested.
	PiResult func(ctx context.Context, id string) string
	// Publish sends the SSE event.
	Publish func(event string, v any)
	// Notify pages the owner (problems only).
	Notify func(ctx context.Context, msg string)
	// Event writes a developer event line.
	Event func(level, text string)
}

// Coordinator runs the rounds.
type Coordinator struct {
	d    Deps
	wake chan struct{}
	// tickMu: one Tick at a time (Run's and a Nudge's; a tick lets go of mu
	// while it asks main for the apps).
	tickMu sync.Mutex

	mu          sync.Mutex
	started     time.Time
	st          state
	forced      bool      // a nudge asked for a round (auto_update off is no reason to wait)
	nudgedBy    string    // who asked
	nudgeID     string    // main's request id: the id of the round this nudge starts
	quick       bool      // a nudge's look on the RPC reader: no network calls
	lastApps    time.Time // last CheckApps
	lastPublish string    // id:phase:reason last published
}

// New builds a coordinator; call Load before Run.
func New(d Deps) *Coordinator {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Compat == (Compat{}) {
		d.Compat = Floors()
	}
	if d.Settle == 0 {
		d.Settle = DefaultSettle
	}
	return &Coordinator{d: d, wake: make(chan struct{}, 1), st: state{Phase: PhaseIdle}, started: d.Now()}
}

// Load reads the persisted round (a missing / broken file = idle).
func (c *Coordinator) Load() {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := os.ReadFile(c.d.Path)
	if err != nil {
		return
	}
	var s state
	if json.Unmarshal(b, &s) != nil || s.Phase == "" {
		c.d.Logger.Warn().Str("path", c.d.Path).Msg("store update: state file unreadable, starting idle")
		return
	}
	c.st = s
}

// Wake asks the loop to look now.
func (c *Coordinator) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Run looks every TickEvery (and on Wake) until ctx ends.
func (c *Coordinator) Run(ctx context.Context) {
	t := time.NewTicker(TickEvery)
	defer t.Stop()
	c.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.wake:
		}
		c.Tick(ctx)
	}
}

// Nudge starts a round now if anything is behind (main's agent.update with
// force, terminals.update_now, the Pi page). It never waits.
//
// id (main's request id, optional) becomes the round's id when this nudge
// starts one, so the Pi's own update reports (agent.update.status) settle
// main's row. The reply is about the Pi: up_to_date when the round has no
// agent (its apps follow), else waiting_for_idle / running.
func (c *Coordinator) Nudge(ctx context.Context, requestedBy, id string) Reply {
	if requestedBy == "" {
		requestedBy = "admin"
	}
	nudgeID := ""
	if idRe.MatchString(id) {
		nudgeID = id
	}
	arm := func() {
		c.mu.Lock()
		c.forced, c.nudgedBy, c.nudgeID = true, requestedBy, nudgeID
		c.st.RetryAfter = time.Time{}
		c.mu.Unlock()
	}
	// A quick look now with what the Pi already has (no call to main or the
	// utility host: this runs on the RPC reader)…
	arm()
	c.mu.Lock()
	c.quick = true
	c.mu.Unlock()
	c.Tick(ctx)
	c.mu.Lock()
	c.quick = false
	rep := c.replyLocked()
	c.mu.Unlock()
	// …then the full one in the background: the manifest and main's APKs
	// fetched now, a round started if anything is behind.
	bg := context.WithoutCancel(ctx)
	safego.Go("storeupdate.nudge", func() {
		if c.d.Manifest != nil {
			c.d.Manifest(bg, true)
		}
		arm()
		c.mu.Lock()
		c.lastApps = time.Time{}
		c.mu.Unlock()
		c.Tick(bg)
	})
	return rep
}

func (c *Coordinator) replyLocked() Reply {
	switch {
	case c.st.Agent == nil, c.st.Phase == PhaseIdle, c.st.Phase == PhaseDone, c.st.Phase == PhaseCancelled:
		return Reply{Accepted: true, State: "up_to_date", ID: c.st.ID}
	case c.st.Phase == PhaseWaiting, c.st.Phase == PhasePreparing:
		return Reply{Accepted: true, State: "waiting_for_idle", ID: c.st.ID}
	}
	return Reply{Accepted: true, State: "running", ID: c.st.ID}
}

// Ready records a device's answer in `preparing`.
func (c *Coordinator) Ready(ctx context.Context, deviceID, id string, ready bool, reason string) error {
	c.mu.Lock()
	if c.st.Phase != PhasePreparing || c.st.ID != id {
		c.mu.Unlock()
		return ErrNotPreparing
	}
	p := c.st.Participants[deviceID]
	if p == nil {
		c.mu.Unlock()
		return ErrNotInRound
	}
	r := ready
	changed := p.Ready == nil || *p.Ready != r || p.Reason != reason
	p.Ready, p.Reason, p.At = &r, trim(reason, 120), c.d.Now().UTC()
	if changed {
		c.saveLocked()
	}
	c.mu.Unlock()
	if ready {
		c.Wake()
	}
	return nil
}

// Locked: a device of kind running version is below this agent's floor.
// Unknown versions are never locked.
func (c *Coordinator) Locked(kind, version string) (bool, string) {
	min := c.d.Compat.Min(kind)
	if min == "" || version == "" {
		return false, min
	}
	name, _ := splitVersion(version)
	if _, ok := semver(name); !ok {
		return false, min
	}
	return less(name, min), min
}

// CachedFor is the APK cached for a device kind (the 426 body).
func (c *Coordinator) CachedFor(kind string) *Release {
	if c.d.Cached == nil {
		return nil
	}
	return c.d.Cached(appOf(kind))
}

// View is GET /api/store-update for dev (nil = the web page).
func (c *Coordinator) View(dev *Device) View {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.viewLocked()
	if dev != nil {
		me := &Me{Kind: dev.Kind, Version: dev.Version}
		me.Locked, _ = c.Locked(dev.Kind, dev.Version)
		if p := c.st.Participants[dev.ID]; p != nil {
			me.InRound = c.st.Phase == PhasePreparing || c.st.Phase == PhaseFrozen ||
				c.st.Phase == PhasePiInstalling || c.st.Phase == PhaseDevicesInstalling
			me.Ready = p.Ready
		}
		target := c.storeReleaseFor(dev.Kind)
		if c.st.Phase == PhaseDevicesInstalling {
			if r := c.roundTarget(dev.Kind); r != nil {
				target = r
			}
		}
		me.Behind = behind(dev.Version, target)
		v.Me = me
	}
	return v
}

func (c *Coordinator) viewLocked() View {
	v := View{ID: c.st.ID, Phase: c.st.Phase, Reason: c.st.Reason, Quibase: c.st.Quibase, Terminal: c.st.Terminal,
		IdleSeconds: IdleSeconds, StoreRelease: c.st.StoreRelease, Deadline: c.st.Deadline,
		Min: MinView{Quibase: c.d.Compat.MinQuibase, Terminal: c.d.Compat.MinTerminal}, Stragglers: c.st.Stragglers}
	if v.Stragglers == nil {
		v.Stragglers = []string{}
	}
	if !c.st.Since.IsZero() {
		s := c.st.Since
		v.Since = &s
	}
	if a := c.st.Agent; a != nil {
		v.Agent = &AgentMove{From: a.From, To: a.To}
	}
	if (v.Phase == PhaseDone || v.Phase == PhaseCancelled) && c.d.Now().Sub(c.st.Since) > FinishedShown {
		v.Phase = PhaseIdle
	}
	return v
}

// Frozen: the store is between `frozen` and the new Pi (devices show
// Updating; alerts about devices going quiet should wait).
func (c *Coordinator) Frozen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.st.Phase {
	case PhaseFrozen, PhasePiInstalling, PhaseDevicesInstalling:
		return true
	}
	return false
}

// Phase is the current phase (tests, health).
func (c *Coordinator) Phase() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.viewLocked().Phase
}

// Tick is one pass of the loop (exported for tests).
func (c *Coordinator) Tick(ctx context.Context) {
	c.tickMu.Lock()
	defer c.tickMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.d.Now()
	switch c.st.Phase {
	case PhaseIdle, PhaseDone, PhaseCancelled, "":
		c.idleLocked(ctx, now)
	case PhaseWaiting:
		c.waitingLocked(ctx, now)
	case PhasePreparing:
		c.preparingLocked(ctx, now)
	case PhaseFrozen:
		c.frozenLocked(ctx, now)
	case PhasePiInstalling:
		c.piInstallingLocked(ctx, now)
	case PhaseDevicesInstalling:
		c.devicesInstallingLocked(ctx, now)
	}
	c.publishLocked()
}

// --- the plan -----------------------------------------------------------------

// plan is what a round would install now.
type plan struct {
	agent    *agentState
	quibase  *Release
	terminal *Release
	waitApps []string // apps whose wanted version isn't cached yet
}

func (p plan) empty() bool {
	return p.agent == nil && p.quibase == nil && p.terminal == nil && len(p.waitApps) == 0
}

func (c *Coordinator) planLocked(ctx context.Context) plan {
	var p plan
	var m Manifest
	rollback := false
	haveManifest := false
	if c.d.Manifest != nil {
		m, haveManifest = c.d.Manifest(ctx, false)
	}
	piOK := c.d.PiUpdate != nil && (c.d.PiSupported == nil || c.d.PiSupported())
	if piOK && haveManifest && m.AgentVersion != "" && m.AgentSHA != "" {
		own := ""
		if c.d.OwnSHA != nil {
			own = c.d.OwnSHA()
		}
		differs := own != "" && !strings.EqualFold(own, m.AgentSHA)
		back := m.RollbackFor(c.d.Version)
		if differs && (newer(m.AgentVersion, c.d.Version) || back) {
			if c.failedBeforeLocked(m.AgentSHA) {
				// That release's Pi update failed: its apps don't go out
				// without it either (one unit) until a newer manifest.
				return plan{}
			}
			p.agent = &agentState{From: c.d.Version, To: m.AgentVersion, FromSHA: own, ToSHA: m.AgentSHA}
			rollback = back
		}
	}
	devices := c.devices(ctx)
	for _, app := range []string{AppQuibase, AppTerminal} {
		kind, want, min := KindTablet, m.Quibase, m.MinQuibase
		if app == AppTerminal {
			kind, want, min = KindTerminal, m.Terminal, m.MinTerminal
		}
		cached := (*Release)(nil)
		if c.d.Cached != nil {
			cached = c.d.Cached(app)
		}
		// The new agent needs at least its floor: wait for it before the Pi moves. A rollback goes to an older
		// agent, whose floor the apps already meet: it never waits for APKs.
		if p.agent != nil && !rollback && min != "" && (cached == nil || less(cached.VersionName, min)) {
			p.waitApps = append(p.waitApps, app+" "+min)
			continue
		}
		var target *Release
		switch {
		case want != "":
			// The manifest names it: exactly that version, never a newer one.
			if cached == nil || cached.VersionName != want {
				if cur := c.storeReleaseFor(kind); cur == nil || cur.VersionName != want {
					if anyBehindName(devices, kind, want) {
						p.waitApps = append(p.waitApps, app+" "+want)
					}
				}
				continue
			}
			target = cached
		case cached != nil:
			target = cached
		}
		if target == nil {
			continue
		}
		if cur := c.storeReleaseFor(kind); cur != nil && cur.VersionCode >= target.VersionCode {
			continue
		}
		if !anyBehind(devices, kind, target) {
			// Nobody runs anything older: the store is on it already.
			c.setStoreReleaseLocked(kind, target)
			continue
		}
		t := *target
		if app == AppQuibase {
			p.quibase = &t
		} else {
			p.terminal = &t
		}
	}
	return p
}

// failedBeforeLocked: the same agent build already failed or rolled back
// in a round (don't loop on it; a newer manifest is tried again).
func (c *Coordinator) failedBeforeLocked(sha string) bool {
	return c.st.FailedAgentSHA != "" && strings.EqualFold(c.st.FailedAgentSHA, sha)
}

func (c *Coordinator) devices(ctx context.Context) []Device {
	if c.d.Devices == nil {
		return nil
	}
	return c.d.Devices(ctx)
}

// --- phases ---------------------------------------------------------------------

func (c *Coordinator) idleLocked(ctx context.Context, now time.Time) {
	if !c.forced && (now.Before(c.st.RetryAfter) || now.Sub(c.started) < c.d.Settle) {
		return
	}
	if c.forced && now.Sub(c.lastApps) >= time.Minute {
		c.checkAppsLocked(ctx, now)
	}
	p := c.planLocked(ctx)
	if p.empty() {
		c.forced = false
		return
	}
	if !c.forced && c.d.AutoOn != nil && !c.d.AutoOn(ctx) {
		return
	}
	by := c.nudgedBy
	if !c.forced {
		by = "auto"
	}
	rid := newID()
	if c.forced && c.nudgeID != "" {
		rid = c.nudgeID
	}
	c.st = state{ID: rid, Phase: PhaseWaiting, Since: now.UTC(), RequestedBy: by,
		StoreRelease: c.st.StoreRelease, FailedAgentSHA: c.st.FailedAgentSHA}
	c.forced = false
	c.applyPlanLocked(p)
	c.event("info", fmt.Sprintf("Store update %s: waiting for a quiet moment (%s)", c.st.ID, c.describeLocked()))
	c.saveLocked()
	c.waitingLocked(ctx, now)
}

func (c *Coordinator) applyPlanLocked(p plan) {
	c.st.Agent, c.st.Quibase, c.st.Terminal = p.agent, p.quibase, p.terminal
	if len(p.waitApps) > 0 {
		c.st.Reason = "waiting_for_apps: " + strings.Join(p.waitApps, ", ")
	}
}

func (c *Coordinator) waitingLocked(ctx context.Context, now time.Time) {
	if c.forced {
		c.forced = false
		c.st.RetryAfter = time.Time{}
	}
	if now.Before(c.st.RetryAfter) {
		return
	}
	if now.Sub(c.lastApps) >= AppsCheckEvery {
		c.checkAppsLocked(ctx, now)
	}
	p := c.planLocked(ctx)
	if p.empty() {
		c.finishLocked(now, PhaseIdle, "")
		return
	}
	c.applyPlanLocked(p)
	if len(p.waitApps) > 0 {
		c.saveIfChangedLocked()
		return
	}
	if why := c.busy(ctx); why != "" {
		c.st.Reason = "busy: " + why
		c.saveIfChangedLocked()
		return
	}
	// The Pi is quiet: ask the devices.
	c.st.Participants = map[string]*participant{}
	for _, d := range c.devices(ctx) {
		if d.Connected && onProtocol(d) {
			c.st.Participants[d.ID] = &participant{Name: d.Name, Kind: d.Kind}
		}
	}
	c.st.Reason = ""
	if len(c.st.Participants) == 0 {
		c.freezeLocked(ctx, now)
		return
	}
	dl := now.Add(PrepareWindow).UTC()
	c.st.Phase, c.st.Since, c.st.Deadline = PhasePreparing, now.UTC(), &dl
	c.event("info", fmt.Sprintf("Store update %s: asking %d device(s) for a quiet moment", c.st.ID, len(c.st.Participants)))
	c.saveLocked()
}

func (c *Coordinator) preparingLocked(ctx context.Context, now time.Time) {
	if why := c.busy(ctx); why != "" {
		c.backToWaitingLocked(now, "busy: "+why, 0)
		return
	}
	on := map[string]bool{}
	for _, d := range c.devices(ctx) {
		if d.Connected {
			on[d.ID] = true
		}
	}
	waiting := []string{}
	for id, p := range c.st.Participants {
		if !on[id] {
			delete(c.st.Participants, id) // went away: excused (the floor / catch-up handles it)
			continue
		}
		if p.Ready == nil || !*p.Ready {
			w := p.Name
			if p.Reason != "" {
				w += " (" + p.Reason + ")"
			}
			waiting = append(waiting, w)
		}
	}
	if len(waiting) == 0 {
		c.freezeLocked(ctx, now)
		return
	}
	if c.st.Deadline != nil && now.After(*c.st.Deadline) {
		sort.Strings(waiting)
		c.backToWaitingLocked(now, "busy: not ready: "+strings.Join(waiting, ", "), BusyRetry)
	}
}

func (c *Coordinator) backToWaitingLocked(now time.Time, reason string, retry time.Duration) {
	c.st.Phase, c.st.Since, c.st.Deadline, c.st.Reason = PhaseWaiting, now.UTC(), nil, trim(reason, 300)
	c.st.Participants = nil
	c.st.RetryAfter = time.Time{}
	if retry > 0 {
		c.st.RetryAfter = now.Add(retry).UTC()
	}
	c.event("info", fmt.Sprintf("Store update %s: not now (%s)", c.st.ID, c.st.Reason))
	c.saveLocked()
}

func (c *Coordinator) freezeLocked(ctx context.Context, now time.Time) {
	c.st.Phase, c.st.Since, c.st.Deadline, c.st.Reason = PhaseFrozen, now.UTC(), nil, ""
	c.saveLocked()
	c.publishLocked() // devices lock before the Pi goes away
	c.frozenLocked(ctx, now)
}

func (c *Coordinator) frozenLocked(ctx context.Context, now time.Time) {
	if c.st.Agent == nil {
		c.devicesPhaseLocked(now)
		return
	}
	if c.d.PiUpdate == nil {
		c.finishLocked(now, PhaseCancelled, "pi_update_not_supported")
		return
	}
	ok, code := c.d.PiUpdate(ctx, c.st.ID, "store-update") // update.sh's fixed list
	if !ok {
		c.finishLocked(now, PhaseCancelled, "pi_update_refused: "+code)
		return
	}
	c.st.PiRequestID = c.st.ID
	c.st.Phase, c.st.Since = PhasePiInstalling, now.UTC()
	c.event("info", fmt.Sprintf("Store update %s: the Pi installs agent %s", c.st.ID, c.st.Agent.To))
	c.saveLocked()
}

func (c *Coordinator) piInstallingLocked(ctx context.Context, now time.Time) {
	a := c.st.Agent
	own := ""
	if c.d.OwnSHA != nil {
		own = c.d.OwnSHA()
	}
	if a == nil || (own != "" && a.ToSHA != "" && strings.EqualFold(own, a.ToSHA)) ||
		(own == "" && c.d.Version != a.From) {
		c.event("info", fmt.Sprintf("Store update %s: the Pi runs agent %s", c.st.ID, c.d.Version))
		c.devicesPhaseLocked(now)
		return
	}
	res := ""
	if c.d.PiResult != nil {
		res = c.d.PiResult(ctx, c.st.PiRequestID)
	}
	switch res {
	case "failed", "rolled_back":
		c.st.FailedAgentSHA = a.ToSHA
		c.finishLocked(now, PhaseCancelled, "pi_update_failed")
		c.notify(ctx, fmt.Sprintf("Store update: the Pi's update to %s didn't work (%s) — the store stays on %s, nothing was installed on the tablets or terminals.", a.To, res, a.From))
		return
	case "up_to_date", "ok":
		// The root updater says so but this binary is still the old one:
		// the manifest moved under us. Nothing to install for the Pi.
		c.devicesPhaseLocked(now)
		return
	}
	if now.Sub(c.st.Since) > PiTimeout {
		c.st.FailedAgentSHA = a.ToSHA
		c.finishLocked(now, PhaseCancelled, "pi_update_failed")
		c.notify(ctx, "Store update: the Pi's update didn't finish in 30 minutes — the store stays on "+a.From+".")
	}
}

func (c *Coordinator) devicesPhaseLocked(now time.Time) {
	if c.st.Quibase != nil {
		c.setStoreReleaseLocked(KindTablet, c.st.Quibase)
	}
	if c.st.Terminal != nil {
		c.setStoreReleaseLocked(KindTerminal, c.st.Terminal)
	}
	if c.st.Quibase == nil && c.st.Terminal == nil {
		c.finishLocked(now, PhaseDone, "")
		return
	}
	c.st.Phase, c.st.Since = PhaseDevicesInstalling, now.UTC()
	c.saveLocked()
}

func (c *Coordinator) devicesInstallingLocked(ctx context.Context, now time.Time) {
	left := []string{}
	for _, d := range c.devices(ctx) {
		if _, in := c.st.Participants[d.ID]; !in {
			continue
		}
		if t := c.roundTarget(d.Kind); t != nil && behind(d.Version, t) {
			left = append(left, d.Name)
		}
	}
	if len(left) == 0 {
		c.finishLocked(now, PhaseDone, "")
		return
	}
	if now.Sub(c.st.Since) > InstallWindow {
		sort.Strings(left)
		c.st.Stragglers = left
		c.finishLocked(now, PhaseDone, "stragglers")
		c.notify(ctx, "Store update: "+strings.Join(left, ", ")+" didn't install the new app in 10 minutes. "+
			"The Pi stays on the new release; each one tries again (it is locked only if it is below the Pi's minimum).")
	}
}

func (c *Coordinator) finishLocked(now time.Time, phase, reason string) {
	if phase == PhaseIdle && c.st.ID == "" {
		return
	}
	c.st.Phase, c.st.Since, c.st.Deadline, c.st.Reason = phase, now.UTC(), nil, reason
	if phase == PhaseCancelled {
		c.st.RetryAfter = now.Add(BusyRetry).UTC() // never a round every 30 s
	}
	if phase == PhaseIdle {
		c.st.Agent, c.st.Quibase, c.st.Terminal, c.st.Participants = nil, nil, nil, nil
	}
	lvl := "info"
	if phase == PhaseCancelled {
		lvl = "warn"
	}
	c.event(lvl, fmt.Sprintf("Store update %s: %s%s", c.st.ID, phase, map[bool]string{true: " (" + reason + ")", false: ""}[reason != ""]))
	c.saveLocked()
}

// --- helpers ---------------------------------------------------------------------

func (c *Coordinator) checkAppsLocked(ctx context.Context, now time.Time) {
	if c.quick {
		return
	}
	c.lastApps = now
	if c.d.CheckApps == nil {
		return
	}
	// Unlocked: the app updaters' checks talk to main (seconds).
	c.mu.Unlock()
	c.d.CheckApps(ctx)
	c.mu.Lock()
}

func (c *Coordinator) busy(ctx context.Context) string {
	if c.d.Busy == nil {
		return ""
	}
	return c.d.Busy(ctx)
}

func (c *Coordinator) storeReleaseFor(kind string) *Release {
	if kind == KindTerminal {
		return c.st.StoreRelease.Terminal
	}
	return c.st.StoreRelease.Quibase
}

func (c *Coordinator) setStoreReleaseLocked(kind string, r *Release) {
	cp := *r
	if kind == KindTerminal {
		c.st.StoreRelease.Terminal = &cp
	} else {
		c.st.StoreRelease.Quibase = &cp
	}
	c.saveLocked()
}

func (c *Coordinator) roundTarget(kind string) *Release {
	if kind == KindTerminal {
		return c.st.Terminal
	}
	return c.st.Quibase
}

func (c *Coordinator) describeLocked() string {
	parts := []string{}
	if a := c.st.Agent; a != nil {
		parts = append(parts, "agent "+a.From+" → "+a.To)
	}
	if r := c.st.Quibase; r != nil {
		parts = append(parts, "Quibase "+r.VersionName)
	}
	if r := c.st.Terminal; r != nil {
		parts = append(parts, "Centurion "+r.VersionName)
	}
	if len(parts) == 0 {
		return c.st.Reason
	}
	return strings.Join(parts, ", ")
}

func (c *Coordinator) event(level, text string) {
	if level == "warn" {
		c.d.Logger.Warn().Msg(text)
	} else {
		c.d.Logger.Info().Msg(text)
	}
	if c.d.Event != nil {
		c.d.Event(level, text)
	}
}

func (c *Coordinator) notify(ctx context.Context, msg string) {
	if c.d.Notify != nil {
		c.d.Notify(ctx, msg)
	}
}

func (c *Coordinator) publishLocked() {
	if c.d.Publish == nil {
		return
	}
	v := c.viewLocked()
	k := v.ID + ":" + v.Phase + ":" + v.Reason
	if k == c.lastPublish {
		return
	}
	c.lastPublish = k
	c.d.Publish(Event, v)
}

func (c *Coordinator) saveIfChangedLocked() { c.saveLocked() }

func (c *Coordinator) saveLocked() {
	if c.d.Path == "" {
		return
	}
	b, err := json.Marshal(c.st)
	if err != nil {
		return
	}
	if err := writeAtomic(c.d.Path, b); err != nil {
		c.d.Logger.Warn().Err(err).Msg("store update: state not saved")
	}
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "su-" + hex.EncodeToString(b)
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func appOf(kind string) string {
	if kind == KindTerminal {
		return AppTerminal
	}
	return AppQuibase
}

// onProtocol: the device's app knows store rounds (it answers ready).
func onProtocol(d Device) bool {
	name, _ := splitVersion(d.Version)
	if _, ok := semver(name); !ok {
		return false
	}
	switch d.Kind {
	case KindTablet:
		return !less(name, ProtocolQuibase)
	case KindTerminal:
		return !less(name, ProtocolTerminal)
	}
	return false
}

func anyBehind(ds []Device, kind string, r *Release) bool {
	for _, d := range ds {
		if d.Kind == kind && d.Version != "" && behind(d.Version, r) {
			return true
		}
	}
	return false
}

func anyBehindName(ds []Device, kind, name string) bool {
	for _, d := range ds {
		if d.Kind != kind || d.Version == "" {
			continue
		}
		n, _ := splitVersion(d.Version)
		if less(n, name) {
			return true
		}
	}
	return false
}

// behind: the device's version is older than r (by version code when both
// have one, else by name). Unknown = not behind.
func behind(version string, r *Release) bool {
	if r == nil || version == "" {
		return false
	}
	name, code := splitVersion(version)
	if code > 0 && r.VersionCode > 0 {
		return code < r.VersionCode
	}
	return less(name, r.VersionName)
}

// splitVersion: "2.1.1+1762990000" → ("2.1.1", 1762990000).
func splitVersion(v string) (string, int64) {
	name, code, _ := strings.Cut(strings.TrimSpace(v), "+")
	n, _ := strconv.ParseInt(code, 10, 64)
	return strings.TrimPrefix(name, "v"), n
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

// less: a < b as x.y.z (a non-semver side = false).
func less(a, b string) bool {
	x, ok1 := semver(a)
	y, ok2 := semver(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}

// newer: the manifest's agent should replace the running one (semver both:
// strictly higher; otherwise any different build, the sha check came first).
func newer(manifest, running string) bool {
	if _, ok := semver(manifest); !ok {
		return manifest != ""
	}
	if _, ok := semver(running); !ok {
		return true
	}
	return less(running, manifest)
}
