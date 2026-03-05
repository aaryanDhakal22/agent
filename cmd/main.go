package main

import (
	"context"
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
)

func main() {
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to load config")
	}

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
	printerService := printerApp.NewService(escposPrinter, logger)
	orderService := orderApp.NewService(printerService, logger)

	consumer := sqsconsumer.NewConsumer(sqsClient, cfg.SQSQueueURL, orderService, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	consumer.Start(ctx)
	logger.Info().Msg("Agent shut down")
}
