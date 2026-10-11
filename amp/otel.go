package amp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

var (
	initOTelMu       sync.Mutex
	initOTelShutdown func(context.Context) error
)

// InitOTel configures the global OpenTelemetry tracer provider to export spans
// to AMP. It is the Go counterpart of amp-instrumentation's init_otel(): it
// reads AMP_OTEL_ENDPOINT and AMP_AGENT_API_KEY from the environment and wires
// a TracerProvider + BatchSpanProcessor + OTLP/HTTP exporter
// (<AMP_OTEL_ENDPOINT>/v1/traces with the x-amp-api-key header). It does no
// instrumentation itself — the caller emits spans, either with
// otel.Tracer(...) directly or with the span helpers on TracerManager.
//
// Idempotent: a second call is a no-op that returns the same shutdown func.
// Call the returned func on exit to flush pending spans.
func InitOTel() (shutdown func(context.Context) error, err error) {
	initOTelMu.Lock()
	defer initOTelMu.Unlock()

	if initOTelShutdown != nil {
		return initOTelShutdown, nil
	}

	cfg := FromEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	tp, err := newTracerProvider(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("amp: %w", err)
	}
	installGlobalTracing(tp)

	initOTelShutdown = tp.Shutdown
	return initOTelShutdown, nil
}

// newTracerProvider builds the TracerProvider that exports to AMP over
// OTLP/HTTP, per the transport half of the manual instrumentation contract.
func newTracerProvider(ctx context.Context, cfg Config) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(cfg.OTLPTracesEndpoint()),
		otlptracehttp.WithHeaders(map[string]string{
			"x-amp-api-key": cfg.AgentAPIKey,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	// Resource attributes matching the Python SDK's behavior.
	var resAttrs []attribute.KeyValue
	if cfg.AgentName != "" {
		resAttrs = append(resAttrs, semconv.ServiceName(cfg.AgentName))
	}
	if cfg.AgentVersion != "" {
		resAttrs = append(resAttrs,
			attribute.String("agent-manager/agent-version", cfg.AgentVersion),
		)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, resAttrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	// BatchSpanProcessor — async export every 5s or 512 spans.
	bsp := sdktrace.NewBatchSpanProcessor(exporter,
		sdktrace.WithBatchTimeout(5*time.Second),
		sdktrace.WithMaxExportBatchSize(512),
	)

	return sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(bsp),
		sdktrace.WithResource(res),
	), nil
}

// tracePropagator carries W3C trace context and baggage; baggage is how an
// evaluation run's task_id / trial_id reach the agent.
func tracePropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

// installGlobalTracing registers tp and the propagator as the OTel globals so
// any OTel-aware library picks them up.
func installGlobalTracing(tp *sdktrace.TracerProvider) {
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(tracePropagator())
}
