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
	"quiccpos/agent/internal/infrastructure/mainclient"
	"quiccpos/agent/internal/infrastructure/notify"
	"quiccpos/agent/internal/infrastructure/printer/escpos"
	sseclient "quiccpos/agent/internal/infrastructure/sse"
	"quiccpos/agent/internal/store"
	"quiccpos/agent/internal/transport"

	"github.com/rs/zerolog"
	"gopkg.in/lumberjack.v2"
)

func main() {
	bootstrap := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().Logger()

	cfg, err := config.Load()
	if err != nil {
		bootstrap.Fatal().Err(err).Msg("Failed to load config")
	}

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
			MaxSize:    100,
			MaxBackups: 10,
			MaxAge:     30,
			Compress:   true,
		}
	default:
		writer = zerolog.ConsoleWriter{Out: os.Stdout}
	}

	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil || cfg.LogLevel == "" {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	logger := zerolog.New(writer).With().Timestamp().Logger()
	logger.Info().
		Str("log_level", level.String()).
		Str("main_server_url", cfg.MainServerURL).
		Str("http_port", cfg.HTTPPort).
		Msg("Agent starting")

	notifier := notify.NewNotifier(cfg.PushoverAppToken, cfg.PushoverUserKey)
	if err := notifier.Send("Agent started", "classical"); err != nil {
		logger.Fatal().Err(err).Msg("Failed to send startup notification")
	}

	pizzaPrinter := escpos.New(cfg.PizzaPrinterIP, "Pizza", logger)
	desiPrinter := escpos.New(cfg.DesiPrinterIP, "Desi", logger)
	subPrinter := escpos.New(cfg.SubPrinterIP, "Sub", logger)
	wingsPrinter := escpos.New(cfg.WingsPrinterIP, "Wings", logger)
	onlinePrinter := escpos.New(cfg.PrinterIP, "Online", logger)

	pizzaService := printerApp.NewService(pizzaPrinter, logger)
	desiService := printerApp.NewService(desiPrinter, logger)
	subService := printerApp.NewService(subPrinter, logger)
	wingsService := printerApp.NewService(wingsPrinter, logger)
	onlineService := printerApp.NewService(onlinePrinter, logger)

	go pizzaService.KeepCheck(cfg.PrinterDetectDelay, notifier)
	go desiService.KeepCheck(cfg.PrinterDetectDelay, notifier)
	go subService.KeepCheck(cfg.PrinterDetectDelay, notifier)
	go wingsService.KeepCheck(cfg.PrinterDetectDelay, notifier)
	go onlineService.KeepCheck(cfg.PrinterDetectDelay, notifier)

	orderService := orderApp.NewService(onlineService, notifier, logger)

	orderStore := store.New()
	mainClient := mainclient.New(cfg.MainServerURL, cfg.AgentAPIKey, logger)
	sseClient := sseclient.New(cfg.MainServerURL, cfg.AgentAPIKey, orderService, orderStore, mainClient, logger)

	httpServer := transport.NewServer(orderStore, orderService, cfg.HTTPPort, logger)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go httpServer.Start(ctx)
	go sseClient.Start(ctx)

	<-ctx.Done()
	logger.Info().Msg("Agent shut down")
}
