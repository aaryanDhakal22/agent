package escpos

import (
	"context"
	"fmt"
	"net"
	"sync"
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

// ESCPOSPrinter is the network handle for one thermal printer. The IP is
// mutable at runtime — mobile flips it via the update endpoint — so every
// read goes through the mutex. Reads happen on the hot paths (Detect, Print),
// writes are rare (manual IP updates), so an RWMutex is the right shape.
type ESCPOSPrinter struct {
	mu     sync.RWMutex
	ip     string
	name   string
	logger zerolog.Logger
	tracer trace.Tracer
}

func New(ip string, name string, logger zerolog.Logger) *ESCPOSPrinter {
	return &ESCPOSPrinter{
		ip:     ip,
		name:   name,
		logger: logger.With().Str("module", "escpos-printer").Str("printer_name", name).Logger(),
		tracer: otel.Tracer(tracerName),
	}
}

func (p *ESCPOSPrinter) Name() string { return p.name }

func (p *ESCPOSPrinter) IP() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ip
}

// SetIP swaps the address used by subsequent Detect / Print calls. In-flight
// dials aren't interrupted — they complete against the old IP and the new IP
// takes effect on the next call.
func (p *ESCPOSPrinter) SetIP(ip string) {
	p.mu.Lock()
	old := p.ip
	p.ip = ip
	p.mu.Unlock()
	p.logger.Info().Str("old_ip", old).Str("new_ip", ip).Msg("Printer IP updated")
}

// Detect checks whether the printer is reachable on port 9100. An empty IP
// short-circuits as ErrPrinterNotConfigured — without this we'd dial ":9100",
// which resolves to localhost and can silently succeed if something else is
// listening there.
func (p *ESCPOSPrinter) Detect(ctx context.Context) error {
	ip := p.IP()
	if ip == "" {
		return fmt.Errorf("%w: %s", printer.ErrPrinterNotConfigured, p.name)
	}
	addr := net.JoinHostPort(ip, printerPort)

	ctx, span := p.tracer.Start(ctx, "escpos.detect",
		trace.WithAttributes(
			attribute.String("printer.name", p.name),
			attribute.String("printer.ip", ip),
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
	ip := p.IP()
	if ip == "" {
		return fmt.Errorf("%w: %s", printer.ErrPrinterNotConfigured, p.name)
	}
	addr := net.JoinHostPort(ip, printerPort)

	ctx, span := p.tracer.Start(ctx, "escpos.write",
		trace.WithAttributes(
			attribute.String("printer.name", p.name),
			attribute.String("printer.ip", ip),
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
