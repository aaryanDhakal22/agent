package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	orderApp "quiccpos/agent/internal/application/order"
	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/config"
	"quiccpos/agent/internal/infrastructure/printer/escpos"
	sqsconsumer "quiccpos/agent/internal/infrastructure/sqs"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/rs/zerolog"
	"gopkg.in/lumberjack.v2"
)

func main() {
	// Temporary bootstrap logger used before the real logger is configured.
	bootstrap := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().Logger()

	// Load config first with a temporary stdout logger for startup errors.
	cfg, err := config.Load()
	if err != nil {
		bootstrap.Fatal().Err(err).Msg("Failed to load config")
	}

	// Determine log output writer.
	var writer io.Writer
	switch cfg.LogOutput {
	case "json":
		writer = os.Stdout
	case "file":
		if err := os.MkdirAll("log", 0o755); err != nil {
			bootstrap.Fatal().Err(err).Msg("Failed to create log directory")
		}
		writer = &lumberjack.Logger{
			Filename:   "log/agent.log",
			MaxSize:    100, // MB
			MaxBackups: 10,
			MaxAge:     30, // days
			Compress:   true,
		}
	default: // "console" or unset
		writer = zerolog.ConsoleWriter{Out: os.Stdout}
	}

	// Determine log level.
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil || cfg.LogLevel == "" {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	logger := zerolog.New(writer).With().Timestamp().Logger()
	logger.Info().
		Str("log_level", level.String()).
		Str("log_output", func() string {
			if cfg.LogOutput == "" {
				return "console"
			}
			return cfg.LogOutput
		}()).
		Msg("Agent starting")

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(cfg.AWSRegion),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AWSAccessKeyID,
			cfg.AWSSecretAccessKey,
			"",
		)),
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to load AWS config")
	}

	sqsClient := sqs.NewFromConfig(awsCfg)

	escposPrinter := escpos.New(cfg.PrinterIP, logger)
	pizzaPrinter := escpos.New(cfg.PizzaPrinterIP, logger)
	printerService := printerApp.NewService(escposPrinter, logger)
	pizzaService := printerApp.NewService(pizzaPrinter, logger)

	go pizzaService.KeepCheck()

	orderService := orderApp.NewService(printerService, logger)

	consumer := sqsconsumer.NewConsumer(sqsClient, cfg.SQSQueueURL, orderService, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	consumer.Start(ctx)
	logger.Info().Msg("Agent shut down")
}
