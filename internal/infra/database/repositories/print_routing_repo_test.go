package repositories

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printroute"
)

// The routing + emergency reroute setting is saved on the Pi, rides in
// agent.state and comes back on a re-imaged Pi's restore (contract §2.3c).
func TestPrintRouting_SaveStateRestore(t *testing.T) {
	ctx := context.Background()
	a := newFloorRepo(t)
	if got, err := a.PrintRouting(ctx); err != nil || got.Mode != printroute.ModeNone {
		t.Fatalf("fresh Pi: %+v %v", got, err)
	}
	st0, err := a.AgentState(ctx)
	if err != nil || st0.PrintRouting != nil {
		t.Fatalf("never set = not in agent.state: %+v %v", st0.PrintRouting, err)
	}

	want := printroute.Routing{Mode: printroute.ModePerPrinter, EmergencyPrinterID: "p-spare",
		Targets: map[string][]string{"p-wings": {"p-spare", "p-pizza"}}}
	if err := a.SetPrintRouting(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.PrintRouting(ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("read back %+v", got)
	}
	arrive(t, a, "pickup")

	stA, err := a.AgentState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stA) // what main keeps (opaque JSON)
	var wire order.AgentState
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.PrintRouting == nil || !reflect.DeepEqual(*wire.PrintRouting, want) {
		t.Fatalf("agent.state print_routing = %+v", wire.PrintRouting)
	}

	b := newFloorRepo(t)
	if _, err := b.Restore(ctx, order.RestoreData{State: &wire}); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.PrintRouting(ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored %+v", got)
	}
}
