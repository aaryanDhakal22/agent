package order

import (
	"fmt"
	"math/rand/v2"
	"strings"

	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infrastructure/notify"

	"github.com/rs/zerolog"
)

type Service struct {
	printerService *printerApp.Service
	notifier       *notify.Notifier
	logger         zerolog.Logger
}

func NewService(printerService *printerApp.Service, notifier *notify.Notifier, logger zerolog.Logger) *Service {
	return &Service{
		printerService: printerService,
		notifier:       notifier,
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

	s.logger.Info().Msg("Sending Notification")
	// Check if the order is paid with cash
	serviceType := formatServiceType(o.ServiceType)
	ntStr := fmt.Sprintf("%s Order : %s", serviceType, customerName)
	if o.Payments == nil {
		// Assume cash payment
		s.notifier.Send(ntStr, "cash-order")
	} else {
		// Assume credit card payment
		// If order is larger than 50 dollars
		if o.OrderTotal > 50 {
			n := rand.IntN(2) + 1
			if n == 1 {
				s.notifier.Send(ntStr, "obama-order")
			} else {
				s.notifier.Send(ntStr, "donald-order")
			}
		} else {
			s.notifier.Send(ntStr, "credit-order")
		}
	}

	s.logger.Info().
		Int("order_id", o.OrderID).
		Str("customer_name", customerName).
		Msg("Order handled successfully")

	return nil
}

func formatServiceType(t string) string {
	switch strings.ToLower(t) {
	case "delivery":
		return "Delivery"
	case "pickup":
		return "Pickup"
	default:
		if t != "" {
			return strings.ToUpper(t)
		}
		return "Online Order"
	}
}
