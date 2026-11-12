package printer

import (
	"context"
	"testing"

	"quiccpos/agent/internal/domain/order"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// CM-7 / CM-12: a rerouted station ticket of a counter order counts on
// orders.tickets.rerouted{source=counter, kind=station}.
func TestReroutedTicketIsCounted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	d, fk := newTestDispatcher(t)
	fk["Pizza"].setDown(true)
	snap := routedSnap()
	snap.Source = order.SourceCounter
	if _, err := d.Execute(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "orders.tickets.rerouted" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				src, _ := dp.Attributes.Value("source")
				kind, _ := dp.Attributes.Value("kind")
				if src.AsString() == "counter" && kind.AsString() == "station" && dp.Value == 1 {
					return
				}
				t.Fatalf("point %v = %d", dp.Attributes.ToSlice(), dp.Value)
			}
		}
	}
	t.Fatal("orders.tickets.rerouted not recorded")
}
