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

	// Order pipeline funnel (SSE inflow → store → print → reprint → trim).
	OrdersReceived         metric.Int64Counter
	OrdersStored           metric.Int64Counter
	OrdersPrintFailed      metric.Int64Counter
	OrdersReprinted        metric.Int64Counter
	OrdersRetentionTrimmed metric.Int64Counter

	// Floor flow (agent-owned states synced to main).
	OrdersEscalated  metric.Int64Counter
	FloorEventsSent  metric.Int64Counter
	FloorSyncBacklog metric.Int64Gauge

	// Duplex RPC channel (main→agent commands, agent→main replies/reports).
	RPCCommandsReceived metric.Int64Counter
	RPCRepliesSent      metric.Int64Counter
	RPCReportsSent      metric.Int64Counter
	RPCHandleMs         metric.Float64Histogram
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

	// Named without a unit suffix: the Collector's prometheusremotewrite
	// exporter appends the unit ("_milliseconds") itself, so "..._ms" would
	// double up as "..._ms_milliseconds" in Prometheus.
	if ms.PrinterDetectMs, err = m.Float64Histogram(
		"printer.detect.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Time to TCP-dial a printer for health check"),
	); err != nil {
		return Meters{}, fmt.Errorf("printer.detect.duration: %w", err)
	}

	if ms.PrinterWriteMs, err = m.Float64Histogram(
		"printer.write.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Time spent writing ESC/POS bytes to a printer"),
	); err != nil {
		return Meters{}, fmt.Errorf("printer.write.duration: %w", err)
	}

	if ms.SSEReconnects, err = m.Int64Counter(
		"sse.reconnects",
		metric.WithDescription("Times the agent reconnected to main/ SSE stream"),
	); err != nil {
		return Meters{}, fmt.Errorf("sse.reconnects: %w", err)
	}

	// --- Order pipeline funnel -------------------------------------------
	if ms.OrdersReceived, err = m.Int64Counter(
		"orders.received",
		metric.WithDescription("Order frames received over the SSE stream (funnel inflow)"),
	); err != nil {
		return Meters{}, fmt.Errorf("orders.received: %w", err)
	}

	if ms.OrdersStored, err = m.Int64Counter(
		"orders.stored",
		metric.WithDescription("Orders persisted to local SQLite (result=ok|error)"),
	); err != nil {
		return Meters{}, fmt.Errorf("orders.stored: %w", err)
	}

	if ms.OrdersPrintFailed, err = m.Int64Counter(
		"orders.print.failed",
		metric.WithDescription("Orders that failed to print (reason=no_printer|write_error)"),
	); err != nil {
		return Meters{}, fmt.Errorf("orders.print.failed: %w", err)
	}

	if ms.OrdersReprinted, err = m.Int64Counter(
		"orders.reprinted",
		metric.WithDescription("Orders reprinted on demand (source=mobile|webui)"),
	); err != nil {
		return Meters{}, fmt.Errorf("orders.reprinted: %w", err)
	}

	if ms.OrdersEscalated, err = m.Int64Counter(
		"orders.escalated",
		metric.WithDescription("Unaccepted-order escalations (Pushover + SSE), one per order per interval"),
	); err != nil {
		return Meters{}, fmt.Errorf("orders.escalated: %w", err)
	}

	if ms.FloorEventsSent, err = m.Int64Counter(
		"floor.events.sent",
		metric.WithDescription("Floor events pushed to main (result=ok|rejected|error|unsupported)"),
	); err != nil {
		return Meters{}, fmt.Errorf("floor.events.sent: %w", err)
	}

	if ms.FloorSyncBacklog, err = m.Int64Gauge(
		"floor.sync.backlog",
		metric.WithDescription("Floor events not yet synced to main"),
	); err != nil {
		return Meters{}, fmt.Errorf("floor.sync.backlog: %w", err)
	}

	if ms.OrdersRetentionTrimmed, err = m.Int64Counter(
		"orders.retention.trimmed",
		metric.WithDescription("Orders deleted by the batch-retention sweeper"),
	); err != nil {
		return Meters{}, fmt.Errorf("orders.retention.trimmed: %w", err)
	}

	// --- Duplex RPC channel ----------------------------------------------
	if ms.RPCCommandsReceived, err = m.Int64Counter(
		"rpc.commands.received",
		metric.WithDescription("main→agent commands handled (result=ok|error|unknown)"),
	); err != nil {
		return Meters{}, fmt.Errorf("rpc.commands.received: %w", err)
	}

	if ms.RPCRepliesSent, err = m.Int64Counter(
		"rpc.replies.sent",
		metric.WithDescription("agent→main command replies POSTed (result=ok|post_error)"),
	); err != nil {
		return Meters{}, fmt.Errorf("rpc.replies.sent: %w", err)
	}

	if ms.RPCReportsSent, err = m.Int64Counter(
		"rpc.reports.sent",
		metric.WithDescription("agent→main unsolicited reports POSTed (result=ok|post_error)"),
	); err != nil {
		return Meters{}, fmt.Errorf("rpc.reports.sent: %w", err)
	}

	// Unitless "ms" suffix omitted from the name — the Collector appends
	// "_milliseconds", same as the printer.* histograms above.
	if ms.RPCHandleMs, err = m.Float64Histogram(
		"rpc.handle.duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Time to decode + dispatch + reply to a main→agent command"),
	); err != nil {
		return Meters{}, fmt.Errorf("rpc.handle.duration: %w", err)
	}

	return ms, nil
}
