package order

import (
	"quiccpos/agent/internal/domain/order"
	printerApp "quiccpos/agent/internal/application/printer"

	"github.com/rs/zerolog"
)

type Service struct {
	printerService *printerApp.Service
	logger         zerolog.Logger
}

func NewService(printerService *printerApp.Service, logger zerolog.Logger) *Service {
	return &Service{
		printerService: printerService,
		logger:         logger.With().Str("service", "order").Logger(),
	}
}

// Handle processes a received order by printing a receipt.
func (s *Service) Handle(o order.OrderRequest) error {
	s.logger.Info().Int("order_id", o.OrderID).Str("service_type", o.ServiceType).Msg("Handling order")

	if err := s.printerService.Print(o); err != nil {
		s.logger.Error().Err(err).Int("order_id", o.OrderID).Msg("Failed to print receipt")
		return err
	}

	return nil
}
