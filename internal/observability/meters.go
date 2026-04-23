package observability

import (
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// MeterName is the scope used when creating instruments across the agent.
const MeterName = "quiccpos/agent"

// Meters bundles the agent's custom metric instruments. Auto instruments
// (http.server.*, HTTP client histograms from otelhttp) are not listed here —
// they come from contrib packages.
type Meters struct {
	OrdersPrinted   metric.Int64Counter
	PrinterStatus   metric.Int64Gauge
	PrinterDetectMs metric.Float64Histogram
	PrinterWriteMs  metric.Float64Histogram
	SSEReconnects   metric.Int64Counter
}

func NewMeters() (Meters, error) {
	m := otel.Meter(MeterName)
	var ms Meters
	var err error

	if ms.OrdersPrinted, err = m.Int64Counter(
		"orders.printed",
		metric.WithDescription("Orders printed by the agent"),
	); err != nil {
		return Meters{}, fmt.Errorf("orders.printed: %w", err)
	}

	if ms.PrinterStatus, err = m.Int64Gauge(
		"printer.status",
		metric.WithDescription("Last-known printer reachability: 1=up, 0=down"),
	); err != nil {
		return Meters{}, fmt.Errorf("printer.status: %w", err)
	}

	if ms.PrinterDetectMs, err = m.Float64Histogram(
		"printer.detect.duration_ms",
		metric.WithUnit("ms"),
		metric.WithDescription("Time to TCP-dial a printer for health check"),
	); err != nil {
		return Meters{}, fmt.Errorf("printer.detect.duration_ms: %w", err)
	}

	if ms.PrinterWriteMs, err = m.Float64Histogram(
		"printer.write.duration_ms",
		metric.WithUnit("ms"),
		metric.WithDescription("Time spent writing ESC/POS bytes to a printer"),
	); err != nil {
		return Meters{}, fmt.Errorf("printer.write.duration_ms: %w", err)
	}

	if ms.SSEReconnects, err = m.Int64Counter(
		"sse.reconnects",
		metric.WithDescription("Times the agent reconnected to main/ SSE stream"),
	); err != nil {
		return Meters{}, fmt.Errorf("sse.reconnects: %w", err)
	}

	return ms, nil
}
