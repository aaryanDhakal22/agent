package printer

import "context"

// Printer handles detection and printing to an ESC/POS thermal printer. The
// network location is mutable at runtime — mobile flips it through the update
// endpoint — so implementations must be safe for concurrent IP reads while a
// probe or print is in flight.
type Printer interface {
	Detect(ctx context.Context) error
	Name() string
	IP() string
	SetIP(ip string)
	Print(ctx context.Context, job PrintJob) error
}

// PrintJob holds the raw ESC/POS command bytes to send to the printer.
type PrintJob struct {
	Commands []byte
}
