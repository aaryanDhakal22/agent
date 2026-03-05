package printer

// Printer handles detection and printing to an ESC/POS thermal printer.
type Printer interface {
	Detect() error
	Print(job PrintJob) error
}

// PrintJob holds the raw ESC/POS command bytes to send to the printer.
type PrintJob struct {
	Commands []byte
}
