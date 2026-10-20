// Package catchup recovers orders the SSE stream missed.
//
// main/ has no replay: an order broadcast while the agent is disconnected (or
// on a stalled stream) is saved in main/ but never reaches the agent. The
// reconciler asks main/ for its newest orders — at startup, right after every
// SSE (re)connect, and every minute — and hands any order this agent doesn't
// have to the order service:
//
//	age <= PrintWindow  (60 min) → normal arrival path (auto-accept prints)
//	age <= TabletWindow (12 h)   → stored as arrived for mobile, never printed
//	older                        → ignored (also keeps old test orders out)
//
// Age is measured from the vendor's submitted_date.
package catchup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "quiccpos/agent/catchup"

	DefaultInterval     = time.Minute
	DefaultPrintWindow  = 60 * time.Minute
	DefaultTabletWindow = 12 * time.Hour
	// main/ lists newest OrderID first; old test orders (ids >= 900M) sort
	// ahead of real ones, so fetch well past a day of real orders.
	DefaultFetchNum = 100
)

// Recoverer is the slice of the order service the reconciler needs.
type Recoverer interface {
	Recover(ctx context.Context, o order.OrderRequest, print bool) (bool, error)
}

type Reconciler struct {
	ordersURL string
	apiKey    string
	svc       Recoverer
	client    *http.Client
	logger    zerolog.Logger
	tracer    trace.Tracer
	trigger   chan struct{}

	Interval     time.Duration
	PrintWindow  time.Duration
	TabletWindow time.Duration
	FetchNum     int
	Now          func() time.Time
}

func New(mainURL, apiKey string, svc Recoverer, logger zerolog.Logger) *Reconciler {
	return &Reconciler{
		ordersURL:    strings.TrimRight(mainURL, "/") + "/api/v1/orders",
		apiKey:       apiKey,
		svc:          svc,
		client:       &http.Client{Timeout: 20 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)},
		logger:       logger.With().Str("module", "catchup").Logger(),
		tracer:       otel.Tracer(tracerName),
		trigger:      make(chan struct{}, 1),
		Interval:     DefaultInterval,
		PrintWindow:  DefaultPrintWindow,
		TabletWindow: DefaultTabletWindow,
		FetchNum:     DefaultFetchNum,
		Now:          time.Now,
	}
}

// Trigger asks for a pass now (e.g. the SSE stream just reconnected). Never
// blocks; triggers that arrive while one is pending collapse into it.
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Run passes once immediately, then on every trigger and every Interval,
// until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	r.logger.Info().
		Str("interval", r.Interval.String()).
		Str("print_window", r.PrintWindow.String()).
		Str("tablet_window", r.TabletWindow.String()).
		Msg("Catch-up reconciler started")

	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	first := true
	for {
		res, err := r.Pass(ctx)
		switch {
		case ctx.Err() != nil:
		case err != nil:
			r.logger.Warn().Err(err).Msg("Catch-up pass failed — retrying next tick")
		case first || res.Recovered() > 0:
			r.logger.Info().Interface("result", res).Msg("Catch-up pass done")
		default:
			r.logger.Debug().Interface("result", res).Msg("Catch-up pass done")
		}
		first = false

		select {
		case <-ctx.Done():
			r.logger.Info().Msg("Catch-up reconciler stopped")
			return
		case <-ticker.C:
		case <-r.trigger:
		}
	}
}

// Result counts one pass. Present = within the window and already stored.
type Result struct {
	Fetched     int `json:"fetched"`
	Present     int `json:"present"`
	Printed     int `json:"printed"`
	TabletOnly  int `json:"tablet_only"`
	TooOld      int `json:"too_old"`
	Unparseable int `json:"unparseable"`
	Failed      int `json:"failed"`
}

func (r Result) Recovered() int { return r.Printed + r.TabletOnly }

// Pass fetches main/'s newest orders and recovers the missing ones.
func (r *Reconciler) Pass(ctx context.Context) (Result, error) {
	ctx, span := r.tracer.Start(ctx, "catchup.pass")
	defer span.End()

	var res Result
	orders, err := r.fetch(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "fetch failed")
		return res, err
	}
	res.Fetched = len(orders)

	now := r.Now()
	oldest := now
	for _, o := range orders {
		submitted, err := ParseSubmitted(o.SubmittedDate)
		if err != nil {
			res.Unparseable++
			r.logger.Debug().Int("order_id", o.OrderID).Str("submitted_date", o.SubmittedDate).Msg("Catch-up: unparseable submitted_date — skipped")
			continue
		}
		if submitted.Before(oldest) {
			oldest = submitted
		}
		age := now.Sub(submitted)
		if age > r.TabletWindow {
			res.TooOld++
			continue
		}
		print := age <= r.PrintWindow

		inserted, err := r.svc.Recover(ctx, o, print)
		switch {
		case err != nil:
			res.Failed++
			span.RecordError(err)
			r.logger.Error().Err(err).Int("order_id", o.OrderID).Msg("Catch-up: failed to recover order")
		case !inserted:
			res.Present++
		default:
			if print {
				res.Printed++
			} else {
				res.TabletOnly++
			}
			r.logger.Info().
				Int("order_id", o.OrderID).
				Int("order_number", o.OrderNumber).
				Str("service_type", o.ServiceType).
				Str("submitted_date", o.SubmittedDate).
				Str("age", age.Round(time.Second).String()).
				Bool("printed", print).
				Msg("Recovered missed order")
		}
	}

	if len(orders) >= r.FetchNum && now.Sub(oldest) <= r.TabletWindow {
		r.logger.Warn().Int("fetched", len(orders)).Msg("Catch-up: every fetched order is inside the window — older misses may not be covered")
	}

	span.SetAttributes(
		attribute.Int("catchup.fetched", res.Fetched),
		attribute.Int("catchup.printed", res.Printed),
		attribute.Int("catchup.tablet_only", res.TabletOnly),
		attribute.Int("catchup.failed", res.Failed),
	)
	return res, nil
}

func (r *Reconciler) fetch(ctx context.Context) ([]order.OrderRequest, error) {
	url := fmt.Sprintf("%s?num=%d", r.ordersURL, r.FetchNum)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", r.apiKey)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("list orders: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var orders []order.OrderRequest
	if err := json.NewDecoder(resp.Body).Decode(&orders); err != nil {
		return nil, fmt.Errorf("decode orders: %w", err)
	}
	return orders, nil
}

var submittedLayouts = []string{
	"2006-01-02T15:04:05-0700", // the vendor's format (e.g. 2026-10-19T21:15:00-0400)
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
}

// ParseSubmitted parses a vendor submitted_date. A value without a zone is
// read as UTC.
func ParseSubmitted(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range submittedLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised submitted_date %q", s)
}
