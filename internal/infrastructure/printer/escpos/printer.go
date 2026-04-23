package escpos

import (
	"context"
	"fmt"
	"net"
	"time"

	"quiccpos/agent/internal/domain/printer"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	printerPort   = "9100"
	detectTimeout = 3 * time.Second
	printTimeout  = 10 * time.Second

	tracerName = "quiccpos/agent/escpos"
)

type ESCPOSPrinter struct {
	ip     string
	name   string
	logger zerolog.Logger
	tracer trace.Tracer
}

func New(ip string, name string, logger zerolog.Logger) *ESCPOSPrinter {
	return &ESCPOSPrinter{
		ip:     ip,
		name:   name,
		logger: logger.With().Str("module", "escpos-printer").Str("printer_name", name).Str("printer_ip", ip).Logger(),
		tracer: otel.Tracer(tracerName),
	}
}

// Detect checks whether the printer is reachable on port 9100.
func (p *ESCPOSPrinter) Detect(ctx context.Context) error {
	addr := net.JoinHostPort(p.ip, printerPort)

	ctx, span := p.tracer.Start(ctx, "escpos.detect",
		trace.WithAttributes(
			attribute.String("printer.name", p.name),
			attribute.String("printer.ip", p.ip),
			attribute.String("net.peer.addr", addr),
		),
	)
	defer span.End()

	p.logger.Debug().Ctx(ctx).Str("addr", addr).Msg("Dialing printer for detection")

	conn, err := net.DialTimeout("tcp", addr, detectTimeout)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "dial failed")
		p.logger.Error().Ctx(ctx).Err(err).Str("addr", addr).Msg("Printer unreachable")
		return fmt.Errorf("%w: %s: %v", printer.ErrPrinterUnreachable, addr, err)
	}
	conn.Close()
	p.logger.Debug().Ctx(ctx).Str("addr", addr).Msg("Printer detected")
	return nil
}

// Print sends the ESC/POS command bytes to the printer over TCP.
func (p *ESCPOSPrinter) Print(ctx context.Context, job printer.PrintJob) error {
	addr := net.JoinHostPort(p.ip, printerPort)

	ctx, span := p.tracer.Start(ctx, "escpos.write",
		trace.WithAttributes(
			attribute.String("printer.name", p.name),
			attribute.String("printer.ip", p.ip),
			attribute.Int("bytes.total", len(job.Commands)),
		),
	)
	defer span.End()

	p.logger.Debug().Ctx(ctx).Str("addr", addr).Msg("Connecting for print job")

	conn, err := net.DialTimeout("tcp", addr, printTimeout)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "dial failed")
		p.logger.Error().Ctx(ctx).Err(err).Str("addr", addr).Msg("Failed to connect to printer")
		return fmt.Errorf("%w: connect: %v", printer.ErrPrintFailed, err)
	}
	defer conn.Close()

	if err := conn.SetWriteDeadline(time.Now().Add(printTimeout)); err != nil {
		p.logger.Warn().Ctx(ctx).Err(err).Msg("Failed to set write deadline")
	}

	n, err := conn.Write(job.Commands)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "write failed")
		span.SetAttributes(attribute.Int("bytes.written", n))
		p.logger.Error().Ctx(ctx).Err(err).Str("addr", addr).Int("bytes", len(job.Commands)).Msg("Failed to write to printer")
		return fmt.Errorf("%w: write: %v", printer.ErrPrintFailed, err)
	}
	span.SetAttributes(attribute.Int("bytes.written", n))

	p.logger.Info().Ctx(ctx).Str("addr", addr).Int("bytes", n).Msg("Receipt sent to printer")
	return nil
}

func (p *ESCPOSPrinter) Name() string {
	return p.name
}
