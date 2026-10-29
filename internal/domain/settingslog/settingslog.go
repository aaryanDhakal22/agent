// Package settingslog is the Pi's settings history (2.3.11): every settings
// PUT that changed something leaves one entry — the section, who did it (a
// paired tablet by name, or the Pi web page) and each changed key old → new.
// Kept on the Pi for [Keep] (GET /api/settings/changes, Quibase developer
// mode → Logs → Settings changes).
package settingslog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Keep is how long an entry stays.
const Keep = 90 * 24 * time.Hour

// maxValue bounds one stored value (a long receipt footer or voice line).
const maxValue = 120

// Who changed a setting.
const (
	ByTablet = "tablet"
	ByWeb    = "web"
)

// By is who made a change.
type By struct {
	Kind     string `json:"kind"` // tablet | web
	Name     string `json:"name"` // the tablet's name, "Pi web page"
	DeviceID string `json:"device_id,omitempty"`
}

// Change is one key, old → new (JSON values; nil = not set).
type Change struct {
	Key  string `json:"key"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

// Entry is one settings change.
type Entry struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Section string    `json:"section"`
	By      By        `json:"by"`
	Changes []Change  `json:"changes"`
}

// Repository stores the history.
type Repository interface {
	// Add stores e and drops entries older than Keep.
	Add(ctx context.Context, e Entry) error
	// List returns the entries since a time, newest first, at most limit.
	List(ctx context.Context, since time.Time, limit int) ([]Entry, error)
}

// Diff compares two values of a settings section by their JSON: nested
// objects are flattened to dotted keys (callout voice_lines.<voice>),
// anything else is compared whole. Keys come out sorted; a key on one side
// only has nil on the other.
func Diff(before, after any) []Change {
	b, a := flatten(before), flatten(after)
	keys := make([]string, 0, len(a)+len(b))
	for k := range b {
		keys = append(keys, k)
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []Change
	for _, k := range keys {
		x, y := b[k], a[k]
		if bytes.Equal(x, y) {
			continue
		}
		out = append(out, Change{Key: k, From: value(x), To: value(y)})
	}
	return out
}

// flatten maps every leaf to its compact JSON.
func flatten(v any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	raw, err := json.Marshal(v)
	if err != nil {
		return out
	}
	var walk func(prefix string, raw json.RawMessage)
	walk = func(prefix string, raw json.RawMessage) {
		var obj map[string]json.RawMessage
		if len(raw) > 0 && raw[0] == '{' && json.Unmarshal(raw, &obj) == nil {
			for k, v := range obj {
				key := k
				if prefix != "" {
					key = prefix + "." + k
				}
				walk(key, v)
			}
			return
		}
		if prefix == "" {
			return // a bare value has no key
		}
		var buf bytes.Buffer
		if json.Compact(&buf, raw) == nil {
			raw = buf.Bytes()
		}
		out[prefix] = raw
	}
	walk("", raw)
	return out
}

// value decodes a leaf for storage (absent or null → nil), a long string cut.
func value(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return string(raw)
	}
	if s, ok := v.(string); ok && utf8.RuneCountInString(s) > maxValue {
		return string([]rune(s)[:maxValue-1]) + "…"
	}
	return v
}

// Text is a one-line summary for the Pi's event log:
// "Kitchen tickets: online_card on → off, online_cash_delivery off → on (tablet Counter)".
func (e Entry) Text() string {
	parts := make([]string, 0, len(e.Changes))
	for _, c := range e.Changes {
		if c.Key == strings.ReplaceAll(e.Section, "-", "_") { // auto-accept's auto_accept: the label says it
			parts = append(parts, fmt.Sprintf("%s → %s", Show(c.From), Show(c.To)))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s → %s", c.Key, Show(c.From), Show(c.To)))
	}
	who := e.By.Name
	if e.By.Kind == ByTablet {
		who = "tablet " + e.By.Name
	}
	return fmt.Sprintf("%s: %s (%s)", SectionLabel(e.Section), strings.Join(parts, ", "), who)
}

// Show prints a value the way staff read it: on/off, — for not set.
func Show(v any) string {
	switch x := v.(type) {
	case nil:
		return "—"
	case bool:
		if x {
			return "on"
		}
		return "off"
	case string:
		if x == "" {
			return `""`
		}
		return x
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// SectionLabel names a section for people.
func SectionLabel(section string) string {
	if l, ok := sectionLabels[section]; ok {
		return l
	}
	return section
}

var sectionLabels = map[string]string{
	"tickets":      "Kitchen tickets",
	"features":     "POS features",
	"times":        "Default times",
	"finishing":    "Finishing",
	"callout":      "Callout",
	"deliveries":   "Deliveries",
	"auto-accept":  "Auto-accept",
	"auto-update":  "Pi auto-update",
	"receipt":      "Receipt",
	"floor":        "Floor",
	"driver-reset": "Driver clock-out",
}
