package repositories

import (
	"context"
	"database/sql"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the OTEL scope for the agent's SQLite calls. Dropping otelpgx
// (pgx-only) means we no longer get per-query spans for free, so each repo
// method opens a short span via dbSpan to keep DB timings visible in Grafana.
const tracerName = "quiccpos/agent/database"

// dbSpan starts a span named "db.<op>" and returns the derived context, the
// span, and an end func that records any error and closes the span. Usage:
//
//	ctx, span, end := dbSpan(ctx, "orders.get_by_id")
//	defer end(&err)
func dbSpan(ctx context.Context, op string) (context.Context, trace.Span, func(errp *error)) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "db."+op,
		trace.WithSpanKind(trace.SpanKindClient),
	)
	return ctx, span, func(errp *error) {
		if errp != nil && *errp != nil {
			span.RecordError(*errp)
			span.SetStatus(codes.Error, (*errp).Error())
		}
		span.End()
	}
}

// tsLayout is the RFC3339 text SQLite writes via
// strftime('%Y-%m-%dT%H:%M:%fZ', 'now') (millisecond precision, UTC 'Z').
// time.RFC3339 parses it fine — Go tolerates the fractional-second suffix.
const tsLayout = time.RFC3339

// parseTS parses a non-null RFC3339 timestamp column.
func parseTS(s string) (time.Time, error) {
	return time.Parse(tsLayout, s)
}

// tsWrite is the layout Go writes timestamps in — identical to SQLite's
// strftime('%Y-%m-%dT%H:%M:%fZ') so text comparisons in SQL stay correct.
const tsWrite = "2006-01-02T15:04:05.000Z07:00"

func fmtTS(t time.Time) string { return t.UTC().Format(tsWrite) }

func nullTS(t time.Time) sql.NullString { return sql.NullString{String: fmtTS(t), Valid: true} }

func nullTSIf(t time.Time, ok bool) sql.NullString {
	if !ok {
		return sql.NullString{}
	}
	return nullTS(t)
}

func optTS(s sql.NullString) (*time.Time, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := parseTS(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
