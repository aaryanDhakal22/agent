package observability

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func intGauge(t *testing.T, m metricdata.Metrics) []metricdata.DataPoint[int64] {
	t.Helper()
	g, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("%s: not an int64 gauge (%T)", m.Name, m.Data)
	}
	return g.DataPoints
}

func attr(set attribute.Set, k string) string {
	v, _ := set.Value(attribute.Key(k))
	return v.Emit()
}

func TestRegisterLiveAndAcceptHistogram(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	hub := func() (HubLink, bool) { return HubLink{}, false }
	if _, err := RegisterLive(Live{
		FloorOpen: func(context.Context) ([]FloorCount, error) {
			return []FloorCount{{State: "new", Source: "online", N: 2}, {State: "cooking", Source: "web", N: 3}}, nil
		},
		TabletsConnected: func() int { return 1 },
		TerminalsConnected: func(context.Context) map[string]int {
			return map[string]int{"Counter A": 1, "Counter B": 0}
		},
		Hub:     func() (HubLink, bool) { return hub() },
		Version: "2.1.1",
	}); err != nil {
		t.Fatal(err)
	}
	ms, err := NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	ms.FloorAcceptSeconds.Record(context.Background(), 42, metricWithSource("online"))

	got := collect(t, reader)

	// terminal.connected (CM-2): each paired terminal, 1/0.
	tc := map[string]int64{}
	for _, p := range intGauge(t, got["terminal.connected"]) {
		tc[attr(p.Attributes, "device")] = p.Value
	}
	if len(tc) != 2 || tc["Counter A"] != 1 || tc["Counter B"] != 0 {
		t.Fatalf("terminal.connected = %v", tc)
	}

	// floor.orders.open: every state x source, zero-filled.
	pts := intGauge(t, got["floor.orders.open"])
	if len(pts) != len(FloorStates)*len(FloorSources) {
		t.Fatalf("floor.orders.open: %d series", len(pts))
	}
	for _, p := range pts {
		st, src := attr(p.Attributes, "state"), attr(p.Attributes, "source")
		want := int64(0)
		switch st + "/" + src {
		case "new/online":
			want = 2
		case "cooking/web":
			want = 3
		}
		if p.Value != want {
			t.Fatalf("floor.orders.open{%s,%s} = %d, want %d", st, src, p.Value, want)
		}
	}
	if pts := intGauge(t, got["floor.tablets.connected"]); len(pts) != 1 || pts[0].Value != 1 {
		t.Fatalf("floor.tablets.connected = %+v", pts)
	}
	if pts := intGauge(t, got["agent.info"]); len(pts) != 1 || pts[0].Value != 1 || attr(pts[0].Attributes, "version") != "2.1.1" {
		t.Fatalf("agent.info = %+v", pts)
	}
	// No hub ever reported: both hub gauges absent.
	if _, ok := got["hub.link.up"]; ok {
		t.Fatal("hub.link.up reported without a hub")
	}
	if _, ok := got["hub.status.age.seconds"]; ok {
		t.Fatal("hub.status.age.seconds reported without a hub")
	}
	h, ok := got["floor.accept.seconds"].Data.(metricdata.Histogram[float64])
	if !ok || len(h.DataPoints) != 1 || h.DataPoints[0].Sum != 42 || attr(h.DataPoints[0].Attributes, "source") != "online" {
		t.Fatalf("floor.accept.seconds = %+v", got["floor.accept.seconds"].Data)
	}

	// A hub that went quiet 2 minutes ago.
	hub = func() (HubLink, bool) { return HubLink{Up: false, Age: 2 * time.Minute}, true }
	got = collect(t, reader)
	if pts := intGauge(t, got["hub.link.up"]); len(pts) != 1 || pts[0].Value != 0 {
		t.Fatalf("hub.link.up = %+v", pts)
	}
	age, ok := got["hub.status.age.seconds"].Data.(metricdata.Gauge[float64])
	if !ok || len(age.DataPoints) != 1 || age.DataPoints[0].Value != 120 {
		t.Fatalf("hub.status.age.seconds = %+v", got["hub.status.age.seconds"].Data)
	}
}

func metricWithSource(src string) metric.RecordOption {
	return metric.WithAttributes(attribute.String("source", src))
}

func TestLiveUptimeSSEAndUpdateGauges(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	connected := false
	var lastOK time.Time
	if _, err := RegisterLive(Live{
		Version:           "2.1.1",
		SSEConnected:      func() bool { return connected },
		Started:           time.Now().Add(-90 * time.Second),
		HostUptime:        func() (float64, bool) { return 3600, true },
		UpdateLastSuccess: func() (time.Time, bool) { return lastOK, !lastOK.IsZero() },
	}); err != nil {
		t.Fatal(err)
	}
	ms, err := NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ms.OrdersPrintSeconds.Record(ctx, 0.8, metric.WithAttributes(attribute.String("result", "ok"), attribute.String("source", "online")))
	ms.UpdateRuns.Add(ctx, 0, metric.WithAttributes(attribute.String("action", "update"), attribute.String("result", "ok")))

	got := collect(t, reader)
	if pts := intGauge(t, got["sse.connected"]); len(pts) != 1 || pts[0].Value != 0 {
		t.Fatalf("sse.connected = %+v", pts)
	}
	up, ok := got["agent.uptime.seconds"].Data.(metricdata.Gauge[float64])
	if !ok || len(up.DataPoints) != 1 || up.DataPoints[0].Value < 89 || up.DataPoints[0].Value > 120 {
		t.Fatalf("agent.uptime.seconds = %+v", got["agent.uptime.seconds"].Data)
	}
	host, ok := got["host.uptime.seconds"].Data.(metricdata.Gauge[float64])
	if !ok || len(host.DataPoints) != 1 || host.DataPoints[0].Value != 3600 {
		t.Fatalf("host.uptime.seconds = %+v", got["host.uptime.seconds"].Data)
	}
	if _, ok := got["agent.update.last_success"]; ok {
		t.Fatal("agent.update.last_success reported with no success known")
	}
	h, ok := got["orders.print.seconds"].Data.(metricdata.Histogram[float64])
	if !ok || len(h.DataPoints) != 1 || h.DataPoints[0].Sum != 0.8 || attr(h.DataPoints[0].Attributes, "result") != "ok" {
		t.Fatalf("orders.print.seconds = %+v", got["orders.print.seconds"].Data)
	}
	runs, ok := got["agent.update.runs"].Data.(metricdata.Sum[int64])
	if !ok || len(runs.DataPoints) != 1 || runs.DataPoints[0].Value != 0 || !runs.IsMonotonic {
		t.Fatalf("agent.update.runs (zero series) = %+v", got["agent.update.runs"].Data)
	}

	connected = true
	lastOK = time.Unix(1793000000, 0)
	got = collect(t, reader)
	if pts := intGauge(t, got["sse.connected"]); len(pts) != 1 || pts[0].Value != 1 {
		t.Fatalf("sse.connected = %+v", pts)
	}
	if pts := intGauge(t, got["agent.update.last_success"]); len(pts) != 1 || pts[0].Value != 1793000000 {
		t.Fatalf("agent.update.last_success = %+v", pts)
	}
}

// 2.4.2: floor.tablets.version is in the callback's instrument list (it
// wasn't: the SDK dropped every point and logged "not registered for
// callback" every collection).
func TestLiveTabletVersions(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	if _, err := RegisterLive(Live{
		Version:        "2.4.2",
		TabletVersions: func() map[string]int { return map[string]int{"2.0.3": 1, "unknown": 2} },
	}); err != nil {
		t.Fatal(err)
	}
	pts := intGauge(t, collect(t, reader)["floor.tablets.version"])
	byVer := map[string]int64{}
	for _, p := range pts {
		byVer[attr(p.Attributes, "app_version")] = p.Value
	}
	if len(byVer) != 2 || byVer["2.0.3"] != 1 || byVer["unknown"] != 2 {
		t.Fatalf("floor.tablets.version = %+v", pts)
	}
}

// 2.5.5: the terminal gauges — one version series per connected terminal
// (device + Centurion version), the connected count, and each reported
// update state (a disconnected terminal keeps its last state).
func TestLiveTerminalApps(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	apps := []TerminalApp{
		{Device: "Counter 1", AppVersion: "1.0.6+1762990000", Connected: true, State: "up_to_date"},
		{Device: "Counter 2", AppVersion: "", Connected: true, State: "waiting_for_tap"},
		{Device: "Back office", AppVersion: "1.0.5+1762900000", Connected: false, State: "failed"},
		{Device: "Spare", Connected: false},
	}
	if _, err := RegisterLive(Live{
		Version:      "2.5.5",
		TerminalApps: func(context.Context) []TerminalApp { return apps },
	}); err != nil {
		t.Fatal(err)
	}
	got := collect(t, reader)
	if pts := intGauge(t, got["floor.terminals.connected"]); len(pts) != 1 || pts[0].Value != 2 {
		t.Fatalf("floor.terminals.connected = %+v", pts)
	}
	ver := map[string]int64{}
	for _, p := range intGauge(t, got["floor.terminals.version"]) {
		ver[attr(p.Attributes, "device")+"="+attr(p.Attributes, "app_version")] = p.Value
	}
	if len(ver) != 2 || ver["Counter 1=1.0.6+1762990000"] != 1 || ver["Counter 2=unknown"] != 1 {
		t.Fatalf("floor.terminals.version = %v", ver)
	}
	st := map[string]int64{}
	for _, p := range intGauge(t, got["floor.terminals.update_state"]) {
		st[attr(p.Attributes, "device")+"="+attr(p.Attributes, "state")] = p.Value
	}
	if len(st) != 3 || st["Counter 1=up_to_date"] != 1 || st["Counter 2=waiting_for_tap"] != 1 || st["Back office=failed"] != 1 {
		t.Fatalf("floor.terminals.update_state = %v", st)
	}

	// No terminals: the count is 0, the per-terminal gauges are absent.
	apps = nil
	got = collect(t, reader)
	if pts := intGauge(t, got["floor.terminals.connected"]); len(pts) != 1 || pts[0].Value != 0 {
		t.Fatalf("floor.terminals.connected (none) = %+v", pts)
	}
	if m, ok := got["floor.terminals.version"]; ok && len(intGauge(t, m)) != 0 {
		t.Fatalf("floor.terminals.version (none) = %+v", m.Data)
	}
}
