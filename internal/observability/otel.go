// Package observability wires OpenTelemetry traces, metrics, and logs for the
// agent. Setup installs global providers and returns a shutdown closure that
// flushes all three pipelines. If the OTLP endpoint is empty, no-op providers
// are installed so the agent still runs with zero observability backend
// reachable (handy for quick local runs and tests).
//
// Transport is OTLP HTTP over HTTPS (443). The home-server Collector sits
// behind Traefik at otlp.quiccpos.com; bearer-token auth is carried on the
// Authorization header, which the SDK reads automatically from the standard
// env var OTEL_EXPORTER_OTLP_HEADERS (e.g. "Authorization=Bearer <token>").
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

type Config struct {
	Endpoint    string // e.g. "https://otlp.quiccpos.com" or "http://localhost:4318"
	ServiceName string
	AppEnv      string
	Version     string
}

type Shutdown func(context.Context) error

func Setup(ctx context.Context, cfg Config) (Shutdown, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.Version),
			semconv.DeploymentEnvironmentName(cfg.AppEnv),
			semconv.ServiceInstanceID(instanceID()),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	url := normalizeEndpoint(cfg.Endpoint)

	traceExp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(url+"/v1/traces"))
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp, sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(tp)

	metricExp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(url+"/v1/metrics"))
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(15*time.Second))),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	logExp, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(url+"/v1/logs"))
	if err != nil {
		return nil, fmt.Errorf("otlp log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	otellog.SetLoggerProvider(lp)

	return func(sctx context.Context) error {
		sctx, cancel := context.WithTimeout(sctx, 10*time.Second)
		defer cancel()
		var errs []error
		if err := tp.Shutdown(sctx); err != nil {
			errs = append(errs, fmt.Errorf("trace shutdown: %w", err))
		}
		if err := mp.Shutdown(sctx); err != nil {
			errs = append(errs, fmt.Errorf("metric shutdown: %w", err))
		}
		if err := lp.Shutdown(sctx); err != nil {
			errs = append(errs, fmt.Errorf("log shutdown: %w", err))
		}
		return errors.Join(errs...)
	}, nil
}

// normalizeEndpoint accepts either a full URL (https://otlp.quiccpos.com) or
// a bare host:port (localhost:4318) and returns a URL with scheme. Bare
// host:port defaults to http:// so local dev against a plain Collector works
// without extra config.
func normalizeEndpoint(ep string) string {
	ep = strings.TrimRight(ep, "/")
	if strings.HasPrefix(ep, "http://") || strings.HasPrefix(ep, "https://") {
		return ep
	}
	return "http://" + ep
}

func instanceID() string {
	if hn, err := os.Hostname(); err == nil && hn != "" {
		return hn
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
