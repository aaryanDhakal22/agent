package printer

import "errors"

var (
	ErrPrinterUnreachable  = errors.New("printer unreachable")
	ErrPrintFailed         = errors.New("print failed")
	ErrPrinterNotConfigured = errors.New("printer IP not configured")
)
