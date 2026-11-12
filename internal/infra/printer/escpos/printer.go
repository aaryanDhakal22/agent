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
	printerPort = "9100"
	// detectTimeout: a busy printer (one connection at a time) can be slow
	// to accept the probe's (L-5: was 3 s).
	detectTimeout = 5 * time.Second
	printTimeout  = 10 * time.Second

	tracerName = "quiccpos/agent/escpos"
)

// ESCPOSPrinter is the network handle for one thermal printer. The IP is
// mutable at runtime — mobile flips it via the update endpoint — so every
// read goes through the mutex. Reads happen on the hot paths (Detect, Print),
// writes are rare (manual IP updates), so an RWMutex is the right shape.
//
// line (R-7) lets one connection at a time reach the printer: concurrent
// accepts, terminal prints and the health probe used to open parallel TCP
// connections to one raw-print port, which many ESC/POS printers drop or
// interleave.
type ESCPOSPrinter struct {
	mu     sync.RWMutex
	ip     string
	name   string
	logger zerolog.Logger
	tracer trace.Tracer
	line   chan struct{}
}

func New(ip string, name string, logger zerolog.Logger) *ESCPOSPrinter {
	return &ESCPOSPrinter{
		ip:     ip,
		name:   name,
		logger: logger.With().Str("module", "escpos-printer").Str("printer_name", name).Logger(),
		tracer: otel.Tracer(tracerName),
		line:   make(chan struct{}, 1),
	}
}

// take waits for the printer's line (or ctx); release with the returned func.
func (p *ESCPOSPrinter) take(ctx context.Context) (func(), error) {
	select {
	case p.line <- struct{}{}:
		return func() { <-p.line }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
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
	addr := dialAddr(ip)

	// The 25 s health probe is no trace of its own (L-5: one root trace per
	// printer per probe); a detect inside a print job is a child span.
	if trace.SpanFromContext(ctx).SpanContext().IsValid() {
		var span trace.Span
		ctx, span = p.tracer.Start(ctx, "escpos.detect",
			trace.WithAttributes(
				attribute.String("printer.name", p.name),
				attribute.String("printer.ip", ip),
				attribute.String("net.peer.addr", addr),
			),
		)
		defer span.End()
	}
	span := trace.SpanFromContext(ctx)

	release, err := p.take(ctx)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", printer.ErrPrinterUnreachable, addr, err)
	}
	defer release()

	conn, err := dial(ctx, detectTimeout, addr)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "dial failed")
		// Debug: a printer that is off logs every probe otherwise (L-5);
		// the health check warns once when it goes down.
		p.logger.Debug().Ctx(ctx).Err(err).Str("addr", addr).Msg("Printer unreachable")
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
	addr := dialAddr(ip)

	ctx, span := p.tracer.Start(ctx, "escpos.write",
		trace.WithAttributes(
			attribute.String("printer.name", p.name),
			attribute.String("printer.ip", ip),
			attribute.Int("bytes.total", len(job.Commands)),
		),
	)
	defer span.End()

	p.logger.Debug().Ctx(ctx).Str("addr", addr).Msg("Connecting for print job")

	release, err := p.take(ctx)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("%w: waiting for the printer: %v", printer.ErrPrintFailed, err)
	}
	defer release()

	conn, err := dial(ctx, printTimeout, addr)
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

// dial opens the TCP connection to a printer (a var: tests count the
// connections open at once).
var dial = func(ctx context.Context, timeout time.Duration, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "tcp", addr)
}

// dialAddr turns a configured printer address into host:port: a bare IP/host
// gets the standard raw-print port 9100; an explicit "host:port" is used as-is
// (fake printers in tests, or a printer behind a port-mapping bridge).
func dialAddr(ip string) string {
	if _, _, err := net.SplitHostPort(ip); err == nil {
		return ip
	}
	return net.JoinHostPort(ip, printerPort)
}
