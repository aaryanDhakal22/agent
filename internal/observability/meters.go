package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
	// FloorAcceptSeconds: arrival (or a future order's re-ring) → accept,
	// by source=online|pos.
	FloorAcceptSeconds metric.Float64Histogram
	// OrdersPrintSeconds: an accept's print job, start → every ticket
	// written or given up (result=ok|partial|failed, source=online|pos).
	OrdersPrintSeconds metric.Float64Histogram

	// Pi updates: one per finished update / rollback run
	// (result=ok|up_to_date|rolled_back|failed, action=update|rollback).
	UpdateRuns metric.Int64Counter

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

	// Unit "s" and a name ending in "seconds": the Collector doesn't append
	// the unit again, so Prometheus sees floor_accept_seconds_*.
	if ms.FloorAcceptSeconds, err = m.Float64Histogram(
		"floor.accept.seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Time from an order's arrival (a future order: its re-ring) to its accept (source=online|pos)"),
		metric.WithExplicitBucketBoundaries(5, 15, 30, 60, 90, 120, 180, 300, 600, 900, 1800),
	); err != nil {
		return Meters{}, fmt.Errorf("floor.accept.seconds: %w", err)
	}

	// Sub-second hop: the dispatcher caps a job at 25 s.
	if ms.OrdersPrintSeconds, err = m.Float64Histogram(
		"orders.print.seconds",
		metric.WithUnit("s"),
		metric.WithDescription("An accept's print job: start to every ticket written or given up (result=ok|partial|failed, source=online|pos)"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10, 15, 20, 30),
	); err != nil {
		return Meters{}, fmt.Errorf("orders.print.seconds: %w", err)
	}

	if ms.UpdateRuns, err = m.Int64Counter(
		"agent.update.runs",
		metric.WithDescription("Finished Pi update / rollback runs (result=ok|up_to_date|rolled_back|failed, action=update|rollback)"),
	); err != nil {
		return Meters{}, fmt.Errorf("agent.update.runs: %w", err)
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

// FloorCount is one floor.orders.open series: State is new|cooking|ready|
// out|scheduled, Source online|pos.
type FloorCount struct {
	State  string
	Source string
	N      int64
}

// FloorStates / FloorSources are every floor.orders.open label value; each
// combination is reported (0 when empty) so dashboards never see "no data".
var (
	FloorStates  = []string{"new", "cooking", "ready", "out", "scheduled"}
	FloorSources = []string{"online", "pos"}
)

// HubLink is the store hub's last report as the gauges see it.
type HubLink struct {
	Up  bool          // reported within the stale window
	Age time.Duration // since the last report
}

// Live feeds the observable gauges, read at every metric export (15 s).
// Any nil func leaves its gauge out.
type Live struct {
	// FloorOpen counts the open batch's in-play orders.
	FloorOpen func(ctx context.Context) ([]FloorCount, error)
	// TabletsConnected: paired tablets on the /api/events stream now.
	TabletsConnected func() int
	// Hub: ok=false when no store hub has ever reported (both hub gauges
	// are then absent).
	Hub func() (HubLink, bool)
	// Version is agent.info's version attribute.
	Version string
	// SSEConnected: the agent's SSE stream to main is open (sse.connected).
	SSEConnected func() bool
	// Started is when the process started (agent.uptime.seconds; zero
	// leaves the gauge out).
	Started time.Time
	// HostUptime: seconds since the machine booted; ok=false leaves
	// host.uptime.seconds out.
	HostUptime func() (float64, bool)
	// UpdateLastSuccess: when the last Pi update succeeded; ok=false (never,
	// or not a Pi) leaves agent.update.last_success out.
	UpdateLastSuccess func() (time.Time, bool)
}

// RegisterLive creates the observable gauges floor.orders.open,
// floor.tablets.connected, hub.link.up, hub.status.age.seconds, agent.info,
// sse.connected, agent.uptime.seconds, host.uptime.seconds and
// agent.update.last_success, and one callback that reports them all. The
// registration lives as long as the meter provider.
func RegisterLive(src Live) (metric.Registration, error) {
	m := otel.Meter(MeterName)

	floorOpen, err := m.Int64ObservableGauge("floor.orders.open",
		metric.WithDescription("Open-batch orders in play by state (new|cooking|ready|out|scheduled) and source (online|pos)"))
	if err != nil {
		return nil, fmt.Errorf("floor.orders.open: %w", err)
	}
	tablets, err := m.Int64ObservableGauge("floor.tablets.connected",
		metric.WithDescription("Paired tablets currently on the agent's SSE stream"))
	if err != nil {
		return nil, fmt.Errorf("floor.tablets.connected: %w", err)
	}
	hubUp, err := m.Int64ObservableGauge("hub.link.up",
		metric.WithDescription("Store hub reported within the stale window: 1=up, 0=down (absent when no hub ever reported)"))
	if err != nil {
		return nil, fmt.Errorf("hub.link.up: %w", err)
	}
	hubAge, err := m.Float64ObservableGauge("hub.status.age.seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Seconds since the store hub's last status report (absent when no hub ever reported)"))
	if err != nil {
		return nil, fmt.Errorf("hub.status.age.seconds: %w", err)
	}
	info, err := m.Int64ObservableGauge("agent.info",
		metric.WithDescription("Always 1; the version attribute is the running agent's version"))
	if err != nil {
		return nil, fmt.Errorf("agent.info: %w", err)
	}
	sseUp, err := m.Int64ObservableGauge("sse.connected",
		metric.WithDescription("The agent's SSE stream to main is open: 1=connected, 0=not (also 0 while unlinked)"))
	if err != nil {
		return nil, fmt.Errorf("sse.connected: %w", err)
	}
	// Unit "s" + a name ending in "seconds": Prometheus keeps the name.
	uptime, err := m.Float64ObservableGauge("agent.uptime.seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Seconds since the agent process started"))
	if err != nil {
		return nil, fmt.Errorf("agent.uptime.seconds: %w", err)
	}
	hostUptime, err := m.Float64ObservableGauge("host.uptime.seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Seconds since the machine booted (/proc/uptime; absent where unreadable)"))
	if err != nil {
		return nil, fmt.Errorf("host.uptime.seconds: %w", err)
	}
	// No unit: a unix timestamp, and the Prometheus name stays
	// agent_update_last_success.
	lastUpdate, err := m.Int64ObservableGauge("agent.update.last_success",
		metric.WithDescription("Unix seconds when the last Pi update finished ok (absent when none is known)"))
	if err != nil {
		return nil, fmt.Errorf("agent.update.last_success: %w", err)
	}
	infoAttrs := metric.WithAttributes(attribute.String("version", src.Version))

	return m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		o.ObserveInt64(info, 1, infoAttrs)
		if src.SSEConnected != nil {
			up := int64(0)
			if src.SSEConnected() {
				up = 1
			}
			o.ObserveInt64(sseUp, up)
		}
		if !src.Started.IsZero() {
			o.ObserveFloat64(uptime, time.Since(src.Started).Seconds())
		}
		if src.HostUptime != nil {
			if v, ok := src.HostUptime(); ok {
				o.ObserveFloat64(hostUptime, v)
			}
		}
		if src.UpdateLastSuccess != nil {
			if at, ok := src.UpdateLastSuccess(); ok {
				o.ObserveInt64(lastUpdate, at.Unix())
			}
		}
		if src.TabletsConnected != nil {
			o.ObserveInt64(tablets, int64(src.TabletsConnected()))
		}
		if src.Hub != nil {
			if h, ok := src.Hub(); ok {
				up := int64(0)
				if h.Up {
					up = 1
				}
				o.ObserveInt64(hubUp, up)
				o.ObserveFloat64(hubAge, h.Age.Seconds())
			}
		}
		if src.FloorOpen != nil {
			counts, err := src.FloorOpen(ctx)
			if err != nil {
				return fmt.Errorf("floor.orders.open: %w", err)
			}
			n := map[[2]string]int64{}
			for _, c := range counts {
				n[[2]string{c.State, c.Source}] += c.N
			}
			for _, st := range FloorStates {
				for _, so := range FloorSources {
					o.ObserveInt64(floorOpen, n[[2]string{st, so}], metric.WithAttributes(
						attribute.String("state", st), attribute.String("source", so)))
				}
			}
		}
		return nil
	}, floorOpen, tablets, hubUp, hubAge, info, sseUp, uptime, hostUptime, lastUpdate)
}
