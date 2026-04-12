package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	AWSRegion          string
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	SQSQueueURL        string
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
}

func Load() (*Config, error) {
	cfg := &Config{
		AWSRegion:          os.Getenv("AWS_REGION"),
		AWSAccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SQSQueueURL:        os.Getenv("SQS_QUEUE_URL"),
		PrinterIP:          os.Getenv("PRINTER_IP"),
		PizzaPrinterIP:     os.Getenv("PIZZA_PRINTER_IP"),
		DesiPrinterIP:      os.Getenv("DESI_PRINTER_IP"),
		SubPrinterIP:       os.Getenv("SUB_PRINTER_IP"),
		WingsPrinterIP:     os.Getenv("WINGS_PRINTER_IP"),
		PrinterDetectDelay: getPrinterDetectDelay(),
		PushoverAppToken:   os.Getenv("PUSHOVER_APP_TOKEN"),
		PushoverUserKey:    os.Getenv("PUSHOVER_USER_KEY"),

		LogLevel:  os.Getenv("LOG_LEVEL"),
		LogOutput: os.Getenv("LOG_OUTPUT"),
	}
	fmt.Printf("cfg: %+v\n", cfg)

	if cfg.AWSRegion == "" {
		return nil, fmt.Errorf("AWS_REGION is required")
	}
	if cfg.AWSAccessKeyID == "" {
		return nil, fmt.Errorf("AWS_ACCESS_KEY_ID is required")
	}
	if cfg.AWSSecretAccessKey == "" {
		return nil, fmt.Errorf("AWS_SECRET_ACCESS_KEY is required")
	}
	if cfg.SQSQueueURL == "" {
		return nil, fmt.Errorf("SQS_QUEUE_URL is required")
	}
	if cfg.PrinterIP == "" {
		return nil, fmt.Errorf("PRINTER_IP is required")
	}

	return cfg, nil
}

func getPrinterDetectDelay() time.Duration {
	if v := os.Getenv("PRINTER_DETECT_DELAY"); v != "" {
		d, err := time.ParseDuration(v + "s")
		if err != nil {
			return time.Second * 25
		}
		return d
	}
	return time.Second * 25
}
