package printer

import "errors"

var (
	ErrPrinterUnreachable   = errors.New("printer unreachable")
	ErrPrintFailed          = errors.New("print failed")
	ErrPrinterNotConfigured = errors.New("printer IP not configured")
	// ErrNoTickets: the plan for this mode has nothing to print (e.g. a
	// kitchen resend of an order with only receipt-only items).
	ErrNoTickets = errors.New("nothing to print for this order")
)
