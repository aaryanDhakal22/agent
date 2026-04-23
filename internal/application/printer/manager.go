package printer

import (
	"context"
	"fmt"
	"strings"

	domainprinter "quiccpos/agent/internal/domain/printer"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const managerTracer = "quiccpos/agent/printer-manager"

// Manager owns the name→Service mapping and coordinates IP updates. It's the
// one place that (a) persists a new IP to the config repo, (b) mutates the
// in-memory printer handle, and (c) fires a synchronous probe so the mobile
// interface sees immediate feedback.
type Manager struct {
	configs  domainprinter.ConfigRepository
	services map[string]*Service
	logger   zerolog.Logger
	tracer   trace.Tracer
}

func NewManager(configs domainprinter.ConfigRepository, services []*Service, logger zerolog.Logger) *Manager {
	idx := make(map[string]*Service, len(services))
	for _, svc := range services {
		idx[svc.Name()] = svc
	}
	return &Manager{
		configs:  configs,
		services: idx,
		logger:   logger.With().Str("module", "printer-manager").Logger(),
		tracer:   otel.Tracer(managerTracer),
	}
}

// Names returns all managed printer names. Used by the startup seed path.
func (m *Manager) Names() []string {
	out := make([]string, 0, len(m.services))
	for name := range m.services {
		out = append(out, name)
	}
	return out
}

// UpdateIP persists the new IP, applies it to the live printer handle, and
// fires an immediate probe so the caller sees whether the new address is
// reachable. Returns ErrUnknownPrinter if `name` isn't registered.
func (m *Manager) UpdateIP(ctx context.Context, name, ip string) error {
	name = strings.TrimSpace(name)
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return fmt.Errorf("ip is required")
	}

	ctx, span := m.tracer.Start(ctx, "printer.update_ip",
		trace.WithAttributes(
			attribute.String("printer.name", name),
			attribute.String("printer.new_ip", ip),
		),
	)
	defer span.End()

	svc, ok := m.services[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPrinter, name)
	}

	if err := m.configs.SetIP(ctx, name, ip); err != nil {
		span.RecordError(err)
		return fmt.Errorf("persist ip: %w", err)
	}

	svc.Printer().SetIP(ip)
	m.logger.Info().Ctx(ctx).Str("printer_name", name).Str("ip", ip).Msg("Printer IP updated — probing new address")

	// Synchronous reprobe bounded by escpos.detectTimeout (3s). Gives the
	// HTTP handler a snapshot that reflects the new IP's reachability so
	// mobile can show a "connected / unreachable" UI immediately.
	svc.ProbeNow(ctx)
	return nil
}

// ErrUnknownPrinter is returned by UpdateIP when the named printer isn't
// one of the ones registered at startup.
var ErrUnknownPrinter = fmt.Errorf("unknown printer")
