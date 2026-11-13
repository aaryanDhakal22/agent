package notify

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
)

// The prod-only guard (2.5.5, the owner 2026-11-13: "I don't want staging
// being able to send me notifications"). Every Pushover the agent sends —
// system events, floor problems, owner alerts, driver routes, the store
// round's pushes — leaves through Notifier.post, and post sends only when
// APP_ENV is "prod". Anywhere else (staging, dev, unset) the message is
// logged at info and dropped, even with keys configured.
//
// PUSHOVER_NONPROD=1 is the one escape hatch, for tests only (a fake
// Pushover via PUSHOVER_API_URL). Never set it in Doppler or on a Pi.

// NonProdEnvVar is the tests-only escape hatch.
const NonProdEnvVar = "PUSHOVER_NONPROD"

// sendAllowed: APP_ENV=prod, or the tests-only escape hatch.
func sendAllowed(appEnv, nonProd string) bool {
	return appEnv == "prod" || strings.TrimSpace(nonProd) == "1"
}

// guardFromEnv reads APP_ENV and PUSHOVER_NONPROD (at construction).
func guardFromEnv() (appEnv string, allowed bool) {
	appEnv = os.Getenv("APP_ENV")
	return appEnv, sendAllowed(appEnv, os.Getenv(NonProdEnvVar))
}

// SetLogger sets where suppressed messages are logged (default: nowhere).
func (n *Notifier) SetLogger(l zerolog.Logger) {
	n.mu.Lock()
	n.logger = l
	n.mu.Unlock()
}

// Sends reports whether this agent sends Pushover at all (APP_ENV=prod or
// the tests-only PUSHOVER_NONPROD=1).
func (n *Notifier) Sends() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.sendAllowed
}

// suppressed logs a message the guard dropped; true = drop it.
func (n *Notifier) suppressed(m Message) bool {
	n.mu.RLock()
	allowed, env, l := n.sendAllowed, n.appEnv, n.logger
	n.mu.RUnlock()
	if allowed {
		return false
	}
	if env == "" {
		env = "unset"
	}
	l.Info().Str("app_env", env).Str("title", m.Title).Str("text", m.Text).
		Msgf("pushover suppressed (APP_ENV=%s): %s", env, headline(m))
	return true
}

// headline is the title, or the text's first line when there is none.
func headline(m Message) string {
	if m.Title != "" {
		return m.Title
	}
	line, _, _ := strings.Cut(m.Text, "\n")
	if len(line) > 80 {
		line = line[:80] + "…"
	}
	return line
}
