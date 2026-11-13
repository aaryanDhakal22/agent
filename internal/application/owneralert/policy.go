package owneralert

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Sender delivers one owner message (notify.Notifier.SendOwner).
type Sender func(ctx context.Context, title, text, sound string) error

// Alert is one owner message on offer.
type Alert struct {
	// Key: the dedup key (type + subject: "printer_down:Pizza").
	Key string
	// Text: what the owner reads. Short: its few words for a summary.
	Text  string
	Short string
	Sound string
}

// Outcome is what Offer did with an alert.
type Outcome int

const (
	// Sent: delivered.
	Sent Outcome = iota
	// Closed: outside opening hours, not sent (offer again while it lasts).
	Closed
	// Cooldown: the same key went out less than Cooldown ago (offer again
	// while it lasts).
	Cooldown
	// Held: over the hourly / daily cap, kept for one summary later.
	Held
	// Failed: Pushover didn't take it (offer again while it lasts).
	Failed
	// Off: owner alerts are switched off, or nobody to send to.
	Off
)

func (o Outcome) String() string {
	return [...]string{"sent", "closed", "cooldown", "held", "failed", "off"}[o]
}

// Done reports whether the incident is dealt with (no need to offer again).
func (o Outcome) Done() bool { return o == Sent || o == Held || o == Off }

// Env is the moment an alert is offered in.
type Env struct {
	Now      time.Time
	Open     bool // inside opening hours (OpenAt)
	Settings Settings
	Title    string // the store's name, the message title
	// OfflineByMain: main pages the owner when this Pi is offline
	// (store_config owner_offline_alert_by_main): no "store offline" from here.
	OfflineByMain bool
}

// state is what survives a restart (StatePath, next to the DB).
type state struct {
	// Sent: when each owner message went out (the last 24 h, for the caps).
	Sent []time.Time `json:"sent"`
	// Keys: the last time each dedup key went out (or was held).
	Keys map[string]time.Time `json:"keys"`
	// Held: what the caps held back, for the summary (oldest first).
	Held []held `json:"held,omitempty"`
	// CloseDay: the store-local date the morning batch check was settled.
	CloseDay string `json:"close_day,omitempty"`
}

type held struct {
	Key   string    `json:"key"`
	Short string    `json:"short"`
	At    time.Time `json:"at"`
}

// Policy is the owner's gate: hours, dedup + cooldown, caps, summary.
type Policy struct {
	mu     sync.Mutex
	st     state
	path   string // "" = memory only (tests)
	send   Sender
	logger zerolog.Logger
}

// StatePath is the policy's state file for an agent DB: next to it, named
// after it (agent.db → agent.owner-alerts.json; the dev roster's agents share
// one folder).
func StatePath(dbPath string) string {
	return strings.TrimSuffix(dbPath, filepath.Ext(dbPath)) + ".owner-alerts.json"
}

// NewPolicy loads the state from path ("" = memory only).
func NewPolicy(path string, send Sender, logger zerolog.Logger) *Policy {
	p := &Policy{send: send, path: path, logger: logger.With().Str("module", "owneralert").Logger(), st: state{Keys: map[string]time.Time{}}}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var st state
			if json.Unmarshal(b, &st) == nil {
				if st.Keys == nil {
					st.Keys = map[string]time.Time{}
				}
				p.st = st
			}
		}
	}
	return p
}

// save writes the state (only after a send or a settled check: rare).
// Called with mu held.
func (p *Policy) save() {
	if p.path == "" {
		return
	}
	b, err := json.Marshal(p.st)
	if err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		p.logger.Warn().Err(err).Msg("owner alert state not saved")
		return
	}
	if err := os.Rename(tmp, p.path); err != nil {
		p.logger.Warn().Err(err).Msg("owner alert state not saved")
	}
}

// prune drops what's older than a day (Keys: two days). Called with mu held.
func (p *Policy) prune(now time.Time) {
	kept := p.st.Sent[:0]
	for _, t := range p.st.Sent {
		if now.Sub(t) < 24*time.Hour {
			kept = append(kept, t)
		}
	}
	p.st.Sent = kept
	for k, t := range p.st.Keys {
		if now.Sub(t) >= 48*time.Hour {
			delete(p.st.Keys, k)
		}
	}
	h := p.st.Held[:0]
	for _, x := range p.st.Held {
		if now.Sub(x.At) < 24*time.Hour {
			h = append(h, x)
		}
	}
	p.st.Held = h
}

// capped reports whether one more message would break a cap. mu held.
func (p *Policy) capped(now time.Time, s Settings) bool {
	hour := 0
	for _, t := range p.st.Sent {
		if now.Sub(t) < time.Hour {
			hour++
		}
	}
	return hour >= s.MaxPerHour || len(p.st.Sent) >= s.MaxPerDay
}

// Offer decides an alert's fate and sends it when it may go out.
func (p *Policy) Offer(ctx context.Context, env Env, a Alert) Outcome {
	if !env.Settings.Enabled {
		return Off
	}
	if !env.Open {
		return Closed
	}
	cool := env.Settings.Cooldown
	if cool < MinCooldown {
		cool = MinCooldown
	}
	p.mu.Lock()
	p.prune(env.Now)
	if last, ok := p.st.Keys[a.Key]; ok && env.Now.Sub(last) < cool {
		p.mu.Unlock()
		return Cooldown
	}
	if p.capped(env.Now, env.Settings) {
		p.st.Keys[a.Key] = env.Now
		dup := false
		for _, h := range p.st.Held {
			dup = dup || h.Key == a.Key
		}
		if !dup {
			p.st.Held = append(p.st.Held, held{Key: a.Key, Short: a.Short, At: env.Now})
		}
		p.save()
		p.mu.Unlock()
		p.logger.Info().Str("key", a.Key).Msg("owner alert held: the hourly/daily cap is reached (one summary later)")
		return Held
	}
	p.mu.Unlock()

	if err := p.send(ctx, env.Title, a.Text, a.Sound); err != nil {
		p.logger.Warn().Err(err).Str("key", a.Key).Msg("owner alert not sent (offered again)")
		return Failed
	}
	p.mu.Lock()
	p.st.Sent = append(p.st.Sent, env.Now)
	p.st.Keys[a.Key] = env.Now
	p.save()
	p.mu.Unlock()
	p.logger.Info().Str("key", a.Key).Msg("owner alert sent")
	return Sent
}

// maxSummaryItems: the summary names this many problems, then "N more".
const maxSummaryItems = 4

// Flush sends the one summary of what the caps held back, once the caps
// allow a message again (inside opening hours).
func (p *Policy) Flush(ctx context.Context, env Env) Outcome {
	if !env.Settings.Enabled {
		return Off
	}
	p.mu.Lock()
	p.prune(env.Now)
	if len(p.st.Held) == 0 || !env.Open || p.capped(env.Now, env.Settings) {
		p.mu.Unlock()
		return Closed
	}
	items := append([]held(nil), p.st.Held...)
	p.mu.Unlock()

	text := SummaryText(items, env.Now)
	if err := p.send(ctx, env.Title, text, ""); err != nil {
		p.logger.Warn().Err(err).Msg("owner alert summary not sent (tried again)")
		return Failed
	}
	p.mu.Lock()
	p.st.Sent = append(p.st.Sent, env.Now)
	// Only what the summary named: something held meanwhile waits.
	rest := p.st.Held[:0]
	for _, h := range p.st.Held {
		named := false
		for _, x := range items {
			named = named || x.Key == h.Key
		}
		if !named {
			rest = append(rest, h)
		}
	}
	p.st.Held = rest
	p.save()
	p.mu.Unlock()
	p.logger.Info().Int("items", len(items)).Msg("owner alert summary sent")
	return Sent
}

// SummaryText is the summary of held alerts.
func SummaryText(items []held, now time.Time) string {
	sort.SliceStable(items, func(i, j int) bool { return items[i].At.Before(items[j].At) })
	var shorts []string
	seen := map[string]bool{}
	for _, h := range items {
		if !seen[h.Short] {
			seen[h.Short] = true
			shorts = append(shorts, h.Short)
		}
	}
	more := 0
	if len(shorts) > maxSummaryItems {
		more = len(shorts) - maxSummaryItems
		shorts = shorts[:maxSummaryItems]
	}
	list := strings.Join(shorts, "; ")
	if more > 0 {
		list += fmt.Sprintf("; and %d more", more)
	}
	n := len(items)
	what := "1 more problem"
	if n != 1 {
		what = fmt.Sprintf("%d more problems", n)
	}
	return fmt.Sprintf("To keep messages few, %s waited: %s. Some may be fixed already - check the tablet.", what, list)
}

// CloseDay / SetCloseDay: the store-local date the morning batch check was
// settled (persisted, so a restart doesn't ask again).
func (p *Policy) CloseDay() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st.CloseDay
}

func (p *Policy) SetCloseDay(day string) {
	p.mu.Lock()
	p.st.CloseDay = day
	p.save()
	p.mu.Unlock()
}

// SentCount is how many owner messages went out in the last 24 h.
func (p *Policy) SentCount(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(now)
	return len(p.st.Sent)
}
