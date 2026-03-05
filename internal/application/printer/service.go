package printer

import (
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
		logger:  logger.With().Str("service", "printer").Logger(),
	}
}

// Print builds ESC/POS commands from the order and sends them to the printer.
func (s *Service) Print(o order.OrderRequest) error {
	if err := s.printer.Detect(); err != nil {
		s.logger.Error().Err(err).Int("order_id", o.OrderID).Msg("Printer not reachable, skipping print")
		return err
	}

	commands := receipt.Build(o)

	if err := s.printer.Print(printer.PrintJob{Commands: commands}); err != nil {
		s.logger.Error().Err(err).Int("order_id", o.OrderID).Msg("Failed to print receipt")
		return err
	}

	s.logger.Info().Int("order_id", o.OrderID).Msg("Receipt printed successfully")
	return nil
}
