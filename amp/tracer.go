package amp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Span attribute keys — the exact contract amp-trace-observer reads.
// See: traces-observer-service/opensearch/process.go DetermineSpanType()
const (
	// Primary classifier — DetermineSpanType checks this first.
	AttrTraceloopSpanKind = "traceloop.span.kind"

	// Agent span attributes
	AttrGenAIAgentName      = "gen_ai.agent.name"
	AttrGenAIConversationID = "gen_ai.conversation.id"

	// LLM span attributes
	AttrGenAISystem             = "gen_ai.system"
	AttrGenAIRequestModel       = "gen_ai.request.model"
	AttrGenAIResponseModel      = "gen_ai.response.model"
	AttrGenAIRequestTemperature = "gen_ai.request.temperature"

	// Token usage — emit both variants; observer prioritizes input_tokens/output_tokens
	AttrGenAIUsageInputTokens      = "gen_ai.usage.input_tokens"
	AttrGenAIUsageOutputTokens     = "gen_ai.usage.output_tokens"
	AttrGenAIUsagePromptTokens     = "gen_ai.usage.prompt_tokens"
	AttrGenAIUsageCompletionTokens = "gen_ai.usage.completion_tokens"

	// Tool span attributes
	AttrGenAIToolName = "gen_ai.tool.name"

	// Content capture (when AMP_TRACE_CONTENT=true)
	AttrGenAIInputMessages  = "gen_ai.input.messages"
	AttrGenAIOutputMessages = "gen_ai.output.messages"
)

// Span kind values — must match DetermineSpanType switch cases exactly.
const (
	SpanKindAgent = "agent"
	SpanKindLLM   = "llm"
	SpanKindTool  = "tool"
)

// TracerManager handles OTLP exporter setup and provides span helper methods.
type TracerManager struct {
	cfg            Config
	logger         *zap.Logger
	tracerProvider *sdktrace.TracerProvider
	tracer         trace.Tracer
	propagator     propagation.TextMapPropagator
}

// NewTracerManager configures the OTLP HTTP exporter, BatchSpanProcessor,
// and TracerProvider. The provider is registered as the global default.
func NewTracerManager(cfg Config, logger *zap.Logger) (*TracerManager, error) {
	ctx := context.Background()

	// Parse endpoint — strip scheme for the OTLP client.
	endpoint := cfg.OTELEndpoint
	insecure := true
	if strings.HasPrefix(endpoint, "https://") {
		endpoint = strings.TrimPrefix(endpoint, "https://")
		insecure = false
	} else {
		endpoint = strings.TrimPrefix(endpoint, "http://")
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithHeaders(map[string]string{
			"x-amp-api-key": cfg.AgentAPIKey,
		}),
	}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	// Build resource attributes matching the Python SDK's behavior.
	resAttrs := []attribute.KeyValue{
		semconv.ServiceName(cfg.AgentName),
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
		sdktrace.WithBatchTimeout(5_000_000_000), // 5s in nanoseconds
		sdktrace.WithMaxExportBatchSize(512),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(bsp),
		sdktrace.WithResource(res),
	)

	// Register as global so any OTel-aware library picks it up.
	otel.SetTracerProvider(tp)

	propagator := propagation.TraceContext{}
	otel.SetTextMapPropagator(propagator)

	tm := &TracerManager{
		cfg:            cfg,
		logger:         logger.Named("tracer"),
		tracerProvider: tp,
		tracer:         tp.Tracer("amp-go", trace.WithInstrumentationVersion("0.1.0")),
		propagator:     propagator,
	}

	logger.Info("OTLP tracer configured",
		zap.String("endpoint", cfg.OTELEndpoint),
		zap.Bool("insecure", insecure),
		zap.Bool("trace_content", cfg.TraceContent),
	)

	return tm, nil
}

// Shutdown flushes any pending spans and releases exporter resources.
func (tm *TracerManager) Shutdown(ctx context.Context) error {
	return tm.tracerProvider.Shutdown(ctx)
}

// Tracer returns the underlying OTel tracer for advanced use cases.
func (tm *TracerManager) Tracer() trace.Tracer {
	return tm.tracer
}

// --- Span Helpers ---

// AgentSpan creates a root agent span. All child spans (LLM, tool) must be
// created from the context passed to fn. The conversation ID is set as a span
// attribute for AMP Console grouping.
func (tm *TracerManager) AgentSpan(
	ctx context.Context,
	name string,
	conversationID string,
	inputMessages []Message,
	fn func(context.Context) error,
) error {
	ctx, span := tm.tracer.Start(ctx, name)
	defer span.End()

	span.SetAttributes(
		attribute.String(AttrTraceloopSpanKind, SpanKindAgent),
		attribute.String(AttrGenAIAgentName, tm.cfg.AgentName),
		attribute.String(AttrGenAIConversationID, conversationID),
	)

	if tm.cfg.TraceContent && len(inputMessages) > 0 {
		if b, err := json.Marshal(inputMessages); err == nil {
			span.SetAttributes(attribute.String(AttrGenAIInputMessages, string(b)))
		}
	}

	err := fn(ctx)

	if err != nil {
		span.RecordError(err)
	}

	return err
}

// LLMRequest contains everything needed to create an LLM span and optionally
// inject W3C traceparent into outbound HTTP calls.
type LLMRequest struct {
	Model       string       // gen_ai.request.model, e.g. "gpt-4o"
	Provider    string       // gen_ai.system, e.g. "openai", "ollama"
	Temperature float64      // gen_ai.request.temperature
	Messages    []Message    // conversation messages
	HTTPClient  *http.Client // optional — for traceparent injection
}

// LLMResult is returned by the function passed to LLMSpan.
type LLMResult struct {
	Content          string
	PromptTokens     int
	CompletionTokens int
	FinishReason     string
	Model            string // actual model used (may differ from requested)
}

// Message represents a chat message in the OpenAI-compatible format.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LLMSpan creates a child LLM span. It sets all gen_ai.* attributes that
// amp-trace-observer needs for classification and token accounting.
// If LLMRequest.HTTPClient is set, W3C traceparent is injected into the
// context for outbound propagation.
func (tm *TracerManager) LLMSpan(
	ctx context.Context,
	req LLMRequest,
	fn func(context.Context) (LLMResult, error),
) (LLMResult, error) {
	spanName := fmt.Sprintf("%s.%s", req.Provider, req.Model)
	ctx, span := tm.tracer.Start(ctx, spanName)
	defer span.End()

	span.SetAttributes(
		attribute.String(AttrTraceloopSpanKind, SpanKindLLM),
		attribute.String(AttrGenAISystem, req.Provider),
		attribute.String(AttrGenAIRequestModel, req.Model),
		attribute.Float64(AttrGenAIRequestTemperature, req.Temperature),
	)

	if tm.cfg.TraceContent && len(req.Messages) > 0 {
		if b, err := json.Marshal(req.Messages); err == nil {
			span.SetAttributes(attribute.String(AttrGenAIInputMessages, string(b)))
		}
	}

	result, err := fn(ctx)

	if err != nil {
		span.RecordError(err)
		return result, err
	}

	// Set response attributes.
	responseModel := result.Model
	if responseModel == "" {
		responseModel = req.Model
	}
	span.SetAttributes(
		attribute.String(AttrGenAIResponseModel, responseModel),
		// Emit both naming conventions for compatibility.
		attribute.Int(AttrGenAIUsageInputTokens, result.PromptTokens),
		attribute.Int(AttrGenAIUsageOutputTokens, result.CompletionTokens),
		attribute.Int(AttrGenAIUsagePromptTokens, result.PromptTokens),
		attribute.Int(AttrGenAIUsageCompletionTokens, result.CompletionTokens),
	)

	if tm.cfg.TraceContent && result.Content != "" {
		outMsg := []Message{{Role: "assistant", Content: result.Content}}
		if b, err := json.Marshal(outMsg); err == nil {
			span.SetAttributes(attribute.String(AttrGenAIOutputMessages, string(b)))
		}
	}

	return result, nil
}

// ToolSpan creates a child tool span. Captures tool input/output as span
// attributes when AMP_TRACE_CONTENT is enabled.
func (tm *TracerManager) ToolSpan(
	ctx context.Context,
	toolName string,
	toolInput any,
	fn func(context.Context) (any, error),
) (any, error) {
	ctx, span := tm.tracer.Start(ctx, toolName)
	defer span.End()

	span.SetAttributes(
		attribute.String(AttrTraceloopSpanKind, SpanKindTool),
		attribute.String(AttrGenAIToolName, toolName),
	)

	if tm.cfg.TraceContent && toolInput != nil {
		if b, err := json.Marshal(toolInput); err == nil {
			span.SetAttributes(attribute.String(AttrGenAIInputMessages, string(b)))
		}
	}

	result, err := fn(ctx)

	if err != nil {
		span.RecordError(err)
		return result, err
	}

	if tm.cfg.TraceContent && result != nil {
		if b, err := json.Marshal(result); err == nil {
			span.SetAttributes(attribute.String(AttrGenAIOutputMessages, string(b)))
		}
	}

	return result, nil
}

// InjectTraceparent injects W3C traceparent/tracestate headers into an HTTP
// request for distributed trace correlation. Call this before sending requests
// to the LLM backend to enable amp-trace-observer to stitch the full trace.
func (tm *TracerManager) InjectTraceparent(ctx context.Context, req *http.Request) {
	tm.propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))
}
