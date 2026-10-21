package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// defaultKeepBatches is how many CLOSED batches the agent keeps locally (plus
// the open one) when AGENT_KEEP_BATCHES is unset. The agent is a cache, not the system of
// record (main/ keeps full history), so a small window is plenty.
const defaultKeepBatches = 3

// defaultDiscoveryPort is the UDP port the Quibase LAN probe is answered on.
const defaultDiscoveryPort = 48080

// Config is the agent's runtime configuration. Printers used to be a fixed
// roster of env-var IPs (PIZZA_PRINTER_IP, DESI_PRINTER_IP, …); they now
// come from main/'s store_config handshake on SSE connect and persist in
// the agent's SQLite DB. Same for store identity (name, address, phone).
type Config struct {
	AppEnv        string
	MainServerURL string
	HTTPPort      string
	// HTTPAltPort (HTTP_ALT_PORT, optional) is a second port serving the same
	// API + web UI — 80 on a Pi, so the address needs no ":8080". Empty = off.
	HTTPAltPort        string
	LogLevel           string // trace|debug|info|warn|error — default "info"
	LogOutput          string // console|json|file — default "console"
	PrinterDetectDelay time.Duration
	PushoverAppToken   string
	PushoverUserKey    string
	// PushoverBossKey (optional) also receives floor problems.
	PushoverBossKey string

	// AgentDBPath is the path to the pure-Go SQLite file (AGENT_DB_PATH).
	// Defaults to ~/.quicc/agent.db.
	AgentDBPath string
	// KeepBatches is how many closed batches are kept locally besides the open
	// one (AGENT_KEEP_BATCHES). Older batches are pruned; main has the full set.
	KeepBatches int
	// DiscoveryPort is the UDP port for the tablet's LAN probe
	// (AGENT_DISCOVERY_PORT, default 48080; 0 disables discovery).
	DiscoveryPort int

	// OpenTelemetry
	OTELEndpoint    string // e.g. "home-server.lan:4317"; empty → noop providers
	OTELServiceName string // default "quicc-agent"
}

func Load() (*Config, error) {
	cfg := &Config{
		AppEnv:             getEnvDefault("APP_ENV", "dev"),
		MainServerURL:      os.Getenv("MAIN_SERVER_URL"),
		HTTPPort:           getEnvDefault("HTTP_PORT", "8080"),
		HTTPAltPort:        getEnvDefault("HTTP_ALT_PORT", ""),
		PrinterDetectDelay: getPrinterDetectDelay(),
		PushoverAppToken:   os.Getenv("PUSHOVER_APP_TOKEN"),
		PushoverUserKey:    os.Getenv("PUSHOVER_USER_KEY"),
		PushoverBossKey:    os.Getenv("PUSHOVER_BOSS_KEY"),
		LogLevel:           os.Getenv("LOG_LEVEL"),
		LogOutput:          os.Getenv("LOG_OUTPUT"),

		AgentDBPath: getAgentDBPath(),
		KeepBatches: getKeepBatches(),

		DiscoveryPort: getDiscoveryPort(),

		OTELEndpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		OTELServiceName: getEnvDefault("OTEL_SERVICE_NAME", "quicc-agent"),
	}

	if cfg.MainServerURL == "" {
		return nil, fmt.Errorf("MAIN_SERVER_URL is required")
	}
	// The store key is NOT an env var — it's minted on-site via the web-UI login
	// and persisted in the agent DB. Empty DB ⇒ agent boots unconfigured.

	return cfg, nil
}

// getAgentDBPath resolves AGENT_DB_PATH, defaulting to ~/.quicc/agent.db (or
// ./agent.db if the home dir can't be determined).
func getAgentDBPath() string {
	if v := os.Getenv("AGENT_DB_PATH"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "agent.db"
	}
	return filepath.Join(home, ".quicc", "agent.db")
}

// getKeepBatches reads AGENT_KEEP_BATCHES, falling back to the default on
// an unset or invalid (non-positive / unparseable) value.
func getKeepBatches() int {
	if v := os.Getenv("AGENT_KEEP_BATCHES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultKeepBatches
}

// getDiscoveryPort reads AGENT_DISCOVERY_PORT: 0 disables discovery, an
// unset or invalid value falls back to the default.
func getDiscoveryPort() int {
	if v := os.Getenv("AGENT_DISCOVERY_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 65535 {
			return n
		}
	}
	return defaultDiscoveryPort
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getPrinterDetectDelay() time.Duration {
	if v := os.Getenv("PRINTER_DETECT_DELAY"); v != "" {
		d, err := time.ParseDuration(v + "s")
		if err != nil {
			return 25 * time.Second
		}
		return d
	}
	return 25 * time.Second
}
