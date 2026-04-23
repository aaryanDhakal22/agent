package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	AppEnv             string
	MainServerURL      string
	AgentAPIKey        string
	HTTPPort           string
	PrinterIP          string
	PizzaPrinterIP     string
	DesiPrinterIP      string
	SubPrinterIP       string
	WingsPrinterIP     string
	LogLevel           string // trace|debug|info|warn|error — default "info"
	LogOutput          string // console|json|file — default "console"
	PrinterDetectDelay time.Duration
	PushoverAppToken   string
	PushoverUserKey    string

	// OpenTelemetry
	OTELEndpoint    string // e.g. "home-server.lan:4317"; empty → noop providers
	OTELServiceName string // default "quicc-agent"
}

func Load() (*Config, error) {
	cfg := &Config{
		AppEnv:             getEnvDefault("APP_ENV", "dev"),
		MainServerURL:      os.Getenv("MAIN_SERVER_URL"),
		AgentAPIKey:        os.Getenv("AGENT_API_KEY"),
		HTTPPort:           getEnvDefault("HTTP_PORT", "8080"),
		PrinterIP:          os.Getenv("PRINTER_IP"),
		PizzaPrinterIP:     os.Getenv("PIZZA_PRINTER_IP"),
		DesiPrinterIP:      os.Getenv("DESI_PRINTER_IP"),
		SubPrinterIP:       os.Getenv("SUB_PRINTER_IP"),
		WingsPrinterIP:     os.Getenv("WINGS_PRINTER_IP"),
		PrinterDetectDelay: getPrinterDetectDelay(),
		PushoverAppToken:   os.Getenv("PUSHOVER_APP_TOKEN"),
		PushoverUserKey:    os.Getenv("PUSHOVER_USER_KEY"),
		LogLevel:           os.Getenv("LOG_LEVEL"),
		LogOutput:          os.Getenv("LOG_OUTPUT"),

		OTELEndpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		OTELServiceName: getEnvDefault("OTEL_SERVICE_NAME", "quicc-agent"),
	}

	if cfg.MainServerURL == "" {
		return nil, fmt.Errorf("MAIN_SERVER_URL is required")
	}
	if cfg.AgentAPIKey == "" {
		return nil, fmt.Errorf("AGENT_API_KEY is required")
	}
	if cfg.PrinterIP == "" {
		return nil, fmt.Errorf("PRINTER_IP is required")
	}

	return cfg, nil
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
