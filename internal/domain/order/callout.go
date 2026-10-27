package order

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Finishing orders, terminal changes to POS checks and the store hub's order
// callout (migration 00019). Contract: contract/agent.md §2.9.8-2.9.10.

// --- finishing -----------------------------------------------------------------

// FinishingSettings: when a ready pickup / counter order is done with. A
// terminal (POS) check finishes when the POS closes it (fixed, shown on the
// tablet); an online pickup finishes OnlinePickupMinutes after it was marked
// ready (0 = never, it waits for Close batch as before).
type FinishingSettings struct {
	OnlinePickupMinutes int `json:"online_pickup_minutes"`
}

// DefaultFinishing: online pickups finish 30 minutes after ready.
func DefaultFinishing() FinishingSettings { return FinishingSettings{OnlinePickupMinutes: 30} }

// Validate checks 0-240 minutes.
func (f FinishingSettings) Validate() error {
	if f.OnlinePickupMinutes < 0 || f.OnlinePickupMinutes > 240 {
		return fmt.Errorf("online_pickup_minutes must be 0-240")
	}
	return nil
}

// DecodeFinishing reads stored finishing settings over the defaults.
func DecodeFinishing(raw []byte) (FinishingSettings, error) {
	f := DefaultFinishing()
	if len(raw) == 0 {
		return f, nil
	}
	err := json.Unmarshal(raw, &f)
	return f, err
}

// --- the order callout -----------------------------------------------------------

// CalloutSettings drive the store hub's "Order ready for ticket number ..."
// on the store speaker (Quibase Settings > Order callout). The hub reads
// them from the feed's settings.changed (and /api/integration/config).
type CalloutSettings struct {
	Enabled bool `json:"enabled"`
	// Voice is a voice id from the hub's list (hub status callout.voices);
	// "" = the hub's default voice.
	Voice string `json:"voice"`
	// Volume of the voice, 0-100 %.
	Volume int `json:"volume"`
	// MusicDip: how far the music (Brave) is turned down while speaking,
	// 0-100 % (0 = not at all, 100 = silent).
	MusicDip int `json:"music_dip"`
	// RestoreSeconds: how long the music takes to come back up, 0-10 s.
	RestoreSeconds float64 `json:"restore_seconds"`
	// Chime: a short chime before the words.
	Chime bool `json:"chime"`
	// Repeat (2.3.4): how many times the phrase is said per callout, 1-3
	// (the chime once at the start, the music down for the whole sequence).
	Repeat int `json:"repeat"`
}

// Callout repeat bounds (CalloutSettings.Repeat).
const (
	MinCalloutRepeat = 1
	MaxCalloutRepeat = 3
)

// DefaultCallout: on, the hub's default voice at 90 %, music down 70 %
// coming back over 1.5 s, chime first, said once.
func DefaultCallout() CalloutSettings {
	return CalloutSettings{Enabled: true, Volume: 90, MusicDip: 70, RestoreSeconds: 1.5, Chime: true, Repeat: 1}
}

// MaxVoiceID bounds CalloutSettings.Voice.
const MaxVoiceID = 64

// Validate checks the ranges.
func (c CalloutSettings) Validate() error {
	switch {
	case c.Volume < 0 || c.Volume > 100:
		return fmt.Errorf("volume must be 0-100")
	case c.MusicDip < 0 || c.MusicDip > 100:
		return fmt.Errorf("music_dip must be 0-100")
	case c.RestoreSeconds < 0 || c.RestoreSeconds > 10:
		return fmt.Errorf("restore_seconds must be 0-10")
	case c.Repeat < MinCalloutRepeat || c.Repeat > MaxCalloutRepeat:
		return fmt.Errorf("repeat must be 1-3")
	}
	return ValidateVoice(c.Voice)
}

// ValidateVoice checks a voice id ("" = the hub's default).
func ValidateVoice(voice string) error {
	if len(voice) > MaxVoiceID || strings.ContainsAny(voice, "/\\ \t\n") {
		return fmt.Errorf("voice must be a voice id from the hub's list")
	}
	return nil
}

// DecodeCallout reads stored callout settings over the defaults (settings
// stored before 2.3.4 have no repeat: said once).
func DecodeCallout(raw []byte) (CalloutSettings, error) {
	c := DefaultCallout()
	if len(raw) == 0 {
		return c, nil
	}
	err := json.Unmarshal(raw, &c)
	if c.Repeat < MinCalloutRepeat || c.Repeat > MaxCalloutRepeat {
		c.Repeat = DefaultCallout().Repeat
	}
	return c, err
}

// Callout test kinds (feed callout.test).
const (
	CalloutTest    = "test"    // TEST CALLOUT with a number
	CalloutPreview = "preview" // PREVIEW a voice
)

// CalloutTestData is a callout.test feed event: speak "Order ready for
// ticket number <number>" now with Voice ("" = the chosen voice), even with
// the callout switched off.
type CalloutTestData struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Number int    `json:"number"`
	Voice  string `json:"voice"`
}

// MaxCalloutNumber: ticket numbers the callout speaks (SoftTouch prints up
// to 4 digits).
const MaxCalloutNumber = 9999

// CalloutRenderData is a callout.render feed event (2.3.2): the tablet's
// PREVIEW "on this tablet" - the hub renders "Order ready for ticket number
// <number>" with Voice ("" = the chosen voice) and uploads the WAV to
// PUT /api/integration/callout-audio/<id>; nothing plays in the store.
type CalloutRenderData struct {
	ID     string `json:"id"`
	Voice  string `json:"voice"`
	Number int    `json:"number"`
}

// --- terminal changes ----------------------------------------------------------------

// ChangeLine is one line added to or removed from a POS check.
type ChangeLine struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
	Kitchen  bool   `json:"kitchen"`
}

// Kinds of a terminal change (PosChange.Kind) by what the order was doing.
const (
	ChangeCooking  = "cooking"         // still on Kitchen
	ChangeReopened = "back_to_cooking" // was ready: back on Kitchen
	ChangeReady    = "ready"           // ready, only removed (stays ready)
	ChangeOut      = "out"             // already out with a driver: alert only
)

// PosChange is a terminal change to a check already on the tablet that
// nobody acknowledged yet. Several changes before OK merge into one (the
// newest id and time, every line).
type PosChange struct {
	ID         string       `json:"id"`
	At         time.Time    `json:"at"`
	Kind       string       `json:"kind"`
	Text       string       `json:"text"`
	Added      []ChangeLine `json:"added"`
	Removed    []ChangeLine `json:"removed"`
	DriverName string       `json:"driver_name,omitempty"`
	// AckedAt: someone pressed OK (the tablets stop ringing). The change is
	// kept so the ticket shows CHANGED until it is ready (or 10 min after
	// OK); a later change starts a new one.
	AckedAt *time.Time `json:"acked_at,omitempty"`
}

// Pending reports whether the change still rings (nobody pressed OK).
func (c *PosChange) Pending() bool { return c != nil && c.AckedAt == nil }

// Merge folds a newer change into this one.
func (c *PosChange) Merge(n PosChange) {
	c.ID, c.At, c.Kind, c.DriverName = n.ID, n.At, n.Kind, n.DriverName
	c.Added = append(c.Added, n.Added...)
	c.Removed = append(c.Removed, n.Removed...)
}

// RenderText builds the bar's line: "#158 changed: +1 Garlic Knots, -1 Coke"
// or, out with a driver, "Abdul already left with #158 - +1 Garlic Knots".
func (c *PosChange) RenderText(number *int64) {
	no := "the order"
	if number != nil {
		no = fmt.Sprintf("#%d", *number)
	}
	var parts []string
	for _, l := range c.Added {
		parts = append(parts, fmt.Sprintf("+%d %s", max(l.Quantity, 1), l.Name))
	}
	for _, l := range c.Removed {
		parts = append(parts, fmt.Sprintf("−%d %s", max(l.Quantity, 1), l.Name))
	}
	what := strings.Join(parts, ", ")
	switch {
	case c.Kind == ChangeOut && c.DriverName != "":
		c.Text = fmt.Sprintf("%s already left with %s — %s", c.DriverName, no, what)
	case c.Kind == ChangeOut:
		c.Text = fmt.Sprintf("%s is already out — %s", no, what)
	default:
		c.Text = fmt.Sprintf("%s changed: %s", no, what)
	}
}

// POSDiff compares a POS check's lines before and after a terminal edit:
// lines (top level) added and removed, matched by the POS line id (pos_ref)
// when both sides carry it, else by name + size occurrence. A quantity
// change on the same line counts as added / removed by the difference.
func POSDiff(before, after []LineItem) (added, removed []ChangeLine, addedRefs map[string]bool, removedItems []LineItem) {
	bk, ak := POSLineKeyPair(before, after)
	old := map[string]LineItem{}
	for i, it := range before {
		old[bk[i]] = it
	}
	addedRefs = map[string]bool{}
	now := map[string]bool{}
	for i, it := range after {
		now[ak[i]] = true
		prev, ok := old[ak[i]]
		switch {
		case !ok:
			added = append(added, ChangeLine{Name: itemLabel(it), Quantity: max(it.Quantity, 1), Kitchen: it.Kitchen})
			addedRefs[ak[i]] = true
		case it.Quantity > prev.Quantity:
			added = append(added, ChangeLine{Name: itemLabel(it), Quantity: it.Quantity - prev.Quantity, Kitchen: it.Kitchen || prev.Kitchen})
		case it.Quantity < prev.Quantity:
			removed = append(removed, ChangeLine{Name: itemLabel(it), Quantity: prev.Quantity - it.Quantity, Kitchen: it.Kitchen || prev.Kitchen})
		}
	}
	for i, it := range before {
		if !now[bk[i]] {
			removed = append(removed, ChangeLine{Name: itemLabel(it), Quantity: max(it.Quantity, 1), Kitchen: it.Kitchen})
			removedItems = append(removedItems, it)
		}
	}
	return added, removed, addedRefs, removedItems
}

// POSLineKeys are the keys POSDiff matches lines by (pos_ref, else name +
// size + occurrence).
func POSLineKeys(items []LineItem) []string { return posLineKeys(items, true) }

// POSLineKeyPair keys both sides of a comparison the same way: by pos_ref
// only when every line on BOTH sides has one, else both by name + size. A
// check stored before its lines carried pos_ref (hub < 0.2.0) compared with
// the same check pulled again with them must not read as every line removed
// and added again (store, 2026-10-26, #314).
func POSLineKeyPair(before, after []LineItem) (b, a []string) {
	refs := allRefs(before) && allRefs(after)
	return posLineKeys(before, refs), posLineKeys(after, refs)
}

func allRefs(items []LineItem) bool {
	for _, it := range items {
		if it.PosRef == "" {
			return false
		}
	}
	return true
}

func posLineKeys(items []LineItem, useRefs bool) []string {
	seen := map[string]int{}
	out := make([]string, len(items))
	for i, it := range items {
		if useRefs && it.PosRef != "" {
			out[i] = "ref:" + it.PosRef
			continue
		}
		k := strings.ToLower(strings.TrimSpace(it.Name)) + "|" + strings.ToLower(strings.TrimSpace(it.Size))
		seen[k]++
		out[i] = fmt.Sprintf("name:%s#%d", k, seen[k])
	}
	return out
}

// AnyKitchen reports whether a change list has a kitchen line.
func AnyKitchen(lines []ChangeLine) bool {
	for _, l := range lines {
		if l.Kitchen {
			return true
		}
	}
	return false
}

func itemLabel(it LineItem) string {
	name := strings.TrimSpace(it.Name)
	if sz := strings.TrimSpace(it.Size); sz != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(sz)) {
		name += " (" + sz + ")"
	}
	return name
}
