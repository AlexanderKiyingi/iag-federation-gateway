// Package otel sets up OpenTelemetry tracing for IAG services. Exporters use
// OTLP/gRPC pointed at OTEL_EXPORTER_OTLP_ENDPOINT (default: otel-collector:4317).
package otel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Config configures the tracer provider.
type Config struct {
	// ServiceName is the resource attribute service.name. Required.
	ServiceName string
	// ServiceVersion is the resource attribute service.version. Optional.
	ServiceVersion string
	// Environment maps to deployment.environment.name. Defaults to APP_ENV or "development".
	Environment string
	// Endpoint is the OTLP gRPC endpoint, e.g. "otel-collector:4317". If empty,
	// reads OTEL_EXPORTER_OTLP_ENDPOINT then falls back to "otel-collector:4317".
	Endpoint string
	// Sampler controls trace sampling. When nil, it is resolved from the standard
	// OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG environment variables (see
	// SamplerFromEnv), falling back to AlwaysSample so local development is
	// unchanged. Production should set OTEL_TRACES_SAMPLER_ARG=0.1 or lower —
	// exporting 100% of spans under real load is a material cost and adds
	// avoidable work to every request.
	Sampler sdktrace.Sampler
}

// Shutdowner flushes and shuts down the tracer provider. The returned value
// from Init implements this; callers should defer Shutdown in main().
type Shutdowner interface {
	Shutdown(ctx context.Context) error
}

// Init creates and registers the global TracerProvider. The gRPC connection
// to the collector is constructed lazily — service startup is NOT blocked on
// collector availability. Spans are buffered by the batch processor and
// flushed once the connection comes up.
func Init(ctx context.Context, cfg Config) (Shutdowner, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("otel: ServiceName is required")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
			endpoint = v
		} else {
			endpoint = "otel-collector:4317"
		}
	}
	env := cfg.Environment
	if env == "" {
		if v := os.Getenv("APP_ENV"); v != "" {
			env = v
		} else {
			env = "development"
		}
	}

	// NewClient is the non-deprecated successor to DialContext; it does NOT
	// block on dial. Connection is established on first RPC.
	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("otel: build collector client %s: %w", endpoint, err)
	}

	exporter, err := otlptrace.New(ctx, otlptracegrpc.NewClient(otlptracegrpc.WithGRPCConn(conn)))
	if err != nil {
		return nil, fmt.Errorf("otel: build exporter: %w", err)
	}

	attrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.ServiceName),
		attribute.String("deployment.environment.name", env),
	}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		return nil, fmt.Errorf("otel: build resource: %w", err)
	}

	sampler := cfg.Sampler
	if sampler == nil {
		sampler = SamplerFromEnv()
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp, nil
}

// SamplerFromEnv resolves a sampler from the standard OpenTelemetry environment
// variables, so sampling is an operational dial rather than a code change:
//
//	OTEL_TRACES_SAMPLER      always_on | always_off | traceidratio |
//	                         parentbased_always_on | parentbased_always_off |
//	                         parentbased_traceidratio
//	OTEL_TRACES_SAMPLER_ARG  sampling ratio in [0,1] for the ratio samplers
//
// Setting only OTEL_TRACES_SAMPLER_ARG is treated as parentbased_traceidratio,
// which is the setting production almost always wants and spells it in one
// variable instead of two.
//
// Anything unparseable falls back to AlwaysSample and does NOT error: a typo in
// a sampling ratio should degrade to "too many traces", never to silently
// blinding the platform.
func SamplerFromEnv() sdktrace.Sampler {
	name := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER")))
	arg := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG"))

	if name == "" {
		if arg == "" {
			return sdktrace.AlwaysSample()
		}
		name = "parentbased_traceidratio"
	}

	ratio := func() (float64, bool) {
		r, err := strconv.ParseFloat(arg, 64)
		if err != nil || r < 0 || r > 1 {
			return 0, false
		}
		return r, true
	}

	switch name {
	case "always_on":
		return sdktrace.AlwaysSample()
	case "always_off":
		return sdktrace.NeverSample()
	case "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample())
	case "traceidratio":
		if r, ok := ratio(); ok {
			return sdktrace.TraceIDRatioBased(r)
		}
	case "parentbased_traceidratio":
		if r, ok := ratio(); ok {
			return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(r))
		}
	}
	return sdktrace.AlwaysSample()
}

// MustInit is Init that panics on error. Use in main() for fail-fast startup.
func MustInit(ctx context.Context, cfg Config) Shutdowner {
	tp, err := Init(ctx, cfg)
	if err != nil {
		panic(err)
	}
	return tp
}

// Tracer returns a Tracer for the named instrumentation library.
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }
