package escpos

import (
	"fmt"
	"net"
	"time"

	"quiccpos/agent/internal/domain/printer"

	"github.com/rs/zerolog"
)

const (
	printerPort    = "9100"
	detectTimeout  = 3 * time.Second
	printTimeout   = 10 * time.Second
)

type ESCPOSPrinter struct {
	ip     string
	logger zerolog.Logger
}

func New(ip string, logger zerolog.Logger) *ESCPOSPrinter {
	return &ESCPOSPrinter{
		ip:     ip,
		logger: logger.With().Str("module", "escpos-printer").Str("printer_ip", ip).Logger(),
	}
}

// Detect checks whether the printer is reachable on port 9100.
func (p *ESCPOSPrinter) Detect() error {
	addr := net.JoinHostPort(p.ip, printerPort)
	p.logger.Debug().Str("addr", addr).Msg("Attempting to detect printer via TCP dial")

	conn, err := net.DialTimeout("tcp", addr, detectTimeout)
	if err != nil {
		p.logger.Error().
			Err(err).
			Str("addr", addr).
			Msg("Printer unreachable")
		return fmt.Errorf("%w: %s: %v", printer.ErrPrinterUnreachable, addr, err)
	}
	conn.Close()
	p.logger.Debug().Str("addr", addr).Msg("Printer detected successfully")
	return nil
}

// Print sends the ESC/POS command bytes to the printer over TCP.
func (p *ESCPOSPrinter) Print(job printer.PrintJob) error {
	addr := net.JoinHostPort(p.ip, printerPort)
	p.logger.Debug().Str("addr", addr).Msg("Connecting to printer for print job")

	conn, err := net.DialTimeout("tcp", addr, printTimeout)
	if err != nil {
		p.logger.Error().
			Err(err).
			Str("addr", addr).
			Msg("Failed to connect to printer")
		return fmt.Errorf("%w: connect: %v", printer.ErrPrintFailed, err)
	}
	defer conn.Close()
	p.logger.Debug().Str("addr", addr).Msg("Connection established")

	p.logger.Debug().Str("addr", addr).Dur("timeout", printTimeout).Msg("Setting write deadline")
	if err := conn.SetWriteDeadline(time.Now().Add(printTimeout)); err != nil {
		p.logger.Warn().Err(err).Msg("Failed to set write deadline")
	}

	p.logger.Debug().Str("addr", addr).Int("bytes", len(job.Commands)).Msg("Writing bytes to printer")
	if _, err := conn.Write(job.Commands); err != nil {
		p.logger.Error().
			Err(err).
			Str("addr", addr).
			Int("bytes", len(job.Commands)).
			Msg("Failed to write to printer")
		return fmt.Errorf("%w: write: %v", printer.ErrPrintFailed, err)
	}

	p.logger.Info().
		Str("addr", addr).
		Int("bytes", len(job.Commands)).
		Msg("Receipt sent to printer successfully")
	return nil
}
