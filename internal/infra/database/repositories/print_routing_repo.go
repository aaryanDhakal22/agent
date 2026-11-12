package repositories

import (
	"context"
	"encoding/json"

	"quiccpos/agent/internal/domain/printroute"
	"quiccpos/agent/internal/infra/database/models"
)

// PrintRouting reads the routing + emergency reroute setting
// (settings.print_routing; ” = mode none).
func (r *OrderRepository) PrintRouting(ctx context.Context) (out printroute.Routing, err error) {
	ctx, _, end := dbSpan(ctx, "settings.print_routing.get")
	defer end(&err)
	raw, err := models.New(r.db).GetPrintRouting(ctx)
	if err != nil {
		return printroute.Default(), err
	}
	return printroute.Decode([]byte(raw))
}

// SetPrintRouting stores the setting whole (the caller validated it).
func (r *OrderRepository) SetPrintRouting(ctx context.Context, p printroute.Routing) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.print_routing.set")
	defer end(&err)
	return setPrintRouting(ctx, models.New(r.db), p)
}

func setPrintRouting(ctx context.Context, q *models.Queries, p printroute.Routing) error {
	if p.Targets == nil {
		p.Targets = map[string][]string{}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return q.SetPrintRouting(ctx, string(b))
}
