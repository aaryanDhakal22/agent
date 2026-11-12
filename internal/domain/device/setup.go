package device

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Setup wizard support (contract/agent.md §2.10y): the admin pairing code a
// counter terminal may pair with, and each paired device's last setup /
// full-check result taken from its diagnostics report.

// DefaultAdminPairCode is the admin pairing code until the store sets one
// (the owner's 1024; a Pi setting, "X means configurable").
const DefaultAdminPairCode = "1024"

// AdminPair is the stored admin pairing code setting. Code "" = the default.
type AdminPair struct {
	Code string `json:"code,omitempty"`
	Off  bool   `json:"off,omitempty"`
}

// Effective is the code that pairs a terminal ("" = turned off).
func (a AdminPair) Effective() string {
	if a.Off {
		return ""
	}
	if a.Code == "" {
		return DefaultAdminPairCode
	}
	return a.Code
}

// ValidAdminCode: 4 to 8 digits.
func ValidAdminCode(s string) bool {
	if len(s) < 4 || len(s) > 8 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// SetupResult is one device's last setup (or full check) as its report said.
type SetupResult struct {
	ReportID string    `json:"report_id"`
	Kind     string    `json:"kind"` // setup | full_check
	Outcome  string    `json:"outcome"`
	Headline string    `json:"headline,omitempty"`
	At       time.Time `json:"at"`
	Failed   []string  `json:"failed,omitempty"`
}

// SetupStore keeps the admin code and the setup results (the settings row).
type SetupStore interface {
	AdminPair(ctx context.Context) (AdminPair, error)
	SetAdminPair(ctx context.Context, a AdminPair) error
	DeviceSetups(ctx context.Context) (map[string]SetupResult, error)
	SetDeviceSetup(ctx context.Context, deviceID string, r SetupResult) error
}

// MaxKeptSetups bounds the stored map (oldest dropped).
const MaxKeptSetups = 50

// SetupResultFromReport reads a report v1 body: ok only for kind setup or
// full_check with an outcome. at falls back to now when created_at is
// missing or unreadable; failed keeps the report's top-level string list.
func SetupResultFromReport(raw []byte, now time.Time) (SetupResult, bool) {
	var doc struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		Outcome   string `json:"outcome"`
		Headline  string `json:"headline"`
		CreatedAt string `json:"created_at"`
		Failed    []any  `json:"failed"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return SetupResult{}, false
	}
	kind := strings.TrimSpace(doc.Kind)
	if (kind != "setup" && kind != "full_check") || strings.TrimSpace(doc.Outcome) == "" {
		return SetupResult{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, doc.CreatedAt)
	if err != nil {
		at = now
	}
	r := SetupResult{ReportID: clip(doc.ID, 64), Kind: kind, Outcome: clip(strings.TrimSpace(doc.Outcome), 40),
		Headline: clip(doc.Headline, 120), At: at.UTC()}
	for _, f := range doc.Failed {
		if s, ok := f.(string); ok && s != "" && len(r.Failed) < 30 {
			r.Failed = append(r.Failed, clip(s, 60))
		}
	}
	return r, true
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
