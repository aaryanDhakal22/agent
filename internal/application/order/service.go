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
		logger:         logger.With().Str("module", "order-service").Logger(),
	}
}

// Handle processes a received order by printing a receipt.
func (s *Service) Handle(o order.OrderRequest) error {
	customerName := o.Customer.FirstName + " " + o.Customer.LastName

	s.logger.Info().
		Int("order_id", o.OrderID).
		Str("customer_name", customerName).
		Str("service_type", o.ServiceType).
		Msg("Handling order")

	s.logger.Info().
		Int("order_id", o.OrderID).
		Str("customer_name", customerName).
		Msg("Sending order to printer service")

	if err := s.printerService.Print(o); err != nil {
		s.logger.Error().
			Err(err).
			Int("order_id", o.OrderID).
			Str("customer_name", customerName).
			Msg("Failed to print receipt")
		return err
	}

	s.logger.Info().
		Int("order_id", o.OrderID).
		Str("customer_name", customerName).
		Msg("Order handled successfully")

	return nil
}
