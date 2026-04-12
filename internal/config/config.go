package config

import (
	"fmt"
	"os"
)

type Config struct {
	AWSRegion          string
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	SQSQueueURL        string
	PrinterIP          string
	PizzaPrinterIP     string
	LogLevel           string // trace|debug|info|warn|error — default "info"
	LogOutput          string // console|json|file — default "console"
}

func Load() (*Config, error) {
	cfg := &Config{
		AWSRegion:          os.Getenv("AWS_REGION"),
		AWSAccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		AWSSecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SQSQueueURL:        os.Getenv("SQS_QUEUE_URL"),
		PrinterIP:          os.Getenv("PRINTER_IP"),
		PizzaPrinterIP:     "192.168.1.116",
		LogLevel:           os.Getenv("LOG_LEVEL"),
		LogOutput:          os.Getenv("LOG_OUTPUT"),
	}

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
