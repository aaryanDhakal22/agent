// Package storeconfig is the agent-side mirror of main/'s SSE handshake
// `store_config` payload. The agent receives one of these on every SSE
// connect, persists it to SQLite, and reads it from there on subsequent
// operations (receipt printing, printer dispatch, etc.).
//
// JSON tags must stay in lockstep with main/'s
// internal/transport/sse/store_context.go — if main grows a field the agent
// silently drops it (extra fields are ignored on unmarshal), but the agent
// won't know about it until this file picks it up too.
package storeconfig

import "time"

// StoreContext is the canonical "starter pack" payload. Empty configs
// deserialize as empty maps (the wire side guarantees `{}` not `null`), so
// callers can read keys without nil-checks.
type StoreContext struct {
	Tenant   TenantContext    `json:"tenant"`
	Location LocationContext  `json:"location"`
	Printers []PrinterContext `json:"printers"`
}

type TenantContext struct {
	ID     string         `json:"id"`
	Slug   string         `json:"slug"`
	Name   string         `json:"name"`
	Config map[string]any `json:"config"`
}

type LocationContext struct {
	ID             string         `json:"id"`
	Slug           string         `json:"slug"`
	Name           string         `json:"name"`
	Timezone       string         `json:"timezone"`
	DailyResetHour int            `json:"daily_reset_hour"`
	Phone          string         `json:"phone"`
	AddressLine1   string         `json:"address_line1"`
	AddressLine2   string         `json:"address_line2"`
	Config         map[string]any `json:"config"`
}

type PrinterContext struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IP   string `json:"ip"`
	// IsReceipt: the location's receipt printer (full customer receipt);
	// every other printer is a kitchen station.
	IsReceipt bool      `json:"is_receipt"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LocationConfigString reads a string key out of location.config with a
// fallback. Keeps receipt + UI code free of repetitive nil-checking.
func (s *StoreContext) LocationConfigString(key, fallback string) string {
	if v, ok := s.Location.Config[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// LocationConfigInt reads an integer key out of location.config. JSON
// numbers unmarshal as float64 in untyped maps, so we accept both.
func (s *StoreContext) LocationConfigInt(key string, fallback int) int {
	switch v := s.Location.Config[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return fallback
}

// TenantConfigBool reads a bool key out of tenant.config with a fallback.
func (s *StoreContext) TenantConfigBool(key string, fallback bool) bool {
	if v, ok := s.Tenant.Config[key].(bool); ok {
		return v
	}
	return fallback
}
