package printer

import (
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infrastructure/printer/receipt"

	"github.com/rs/zerolog"
)

type Service struct {
	printer printer.Printer
	logger  zerolog.Logger
}

func NewService(p printer.Printer, logger zerolog.Logger) *Service {
	return &Service{
		printer: p,
		logger:  logger.With().Str("module", "printer-service").Logger(),
	}
}

func (s *Service) KeepCheck() error {
	for {
		s.logger.Info().Msg("Detecting printer availability : ")
		if err := s.printer.Detect(); err != nil {
			s.logger.Error().Err(err).Msg("Printer not reachable, Please check your printer")
			continue
		}
		s.logger.Info().Msg("Printer reachable")
		time.Sleep(time.Second * 10)
	}
}

// Print builds ESC/POS commands from the order and sends them to the printer.
func (s *Service) Print(o order.OrderRequest) error {
	s.logger.Info().Int("order_id", o.OrderID).Msg("Detecting printer availability")

	if err := s.printer.Detect(); err != nil {
		s.logger.Error().Err(err).Int("order_id", o.OrderID).Msg("Printer not reachable, skipping print")
		return err
	}

	s.logger.Debug().Int("order_id", o.OrderID).Msg("Printer reachable")

	s.logger.Debug().Int("order_id", o.OrderID).Msg("Building receipt commands")
	commands := receipt.Build(o)

	s.logger.Info().Int("order_id", o.OrderID).Int("bytes", len(commands)).Msg("Sending print job to printer")

	if err := s.printer.Print(printer.PrintJob{Commands: commands}); err != nil {
		s.logger.Error().Err(err).Int("order_id", o.OrderID).Msg("Failed to print receipt")
		return err
	}

	s.logger.Info().Int("order_id", o.OrderID).Msg("Receipt printed successfully")
	return nil
}
