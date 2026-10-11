package amp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Span attribute keys — AMP's manual instrumentation contract, i.e. exactly
// what agent-manager-observer reads. The contract is layered:
//
//   - Layer 1: OpenTelemetry GenAI semantic conventions (gen_ai.*, plus db.*
//     for retriever spans). The primary set.
//   - Layer 2: OpenLLMetry / Traceloop extension keys (traceloop.*) for what
//     OTel has not standardized yet: the chain span kind, rerank, and
//     tool-call arguments / result.
//
// See: https://wso2.github.io/agent-manager/docs/latest/guides/amp-instrumentation/#the-contract
const (
	// Primary classifier for every GenAI span.
	AttrGenAIOperationName = "gen_ai.operation.name"
	// Provider id on llm/embedding spans, framework on agent spans.
	AttrGenAISystem = "gen_ai.system"

	// Agent span attributes
	AttrGenAIAgentName          = "gen_ai.agent.name"
	AttrGenAIAgentDescription   = "gen_ai.agent.description"
	AttrGenAIAgentTools         = "gen_ai.agent.tools"
	AttrGenAISystemInstructions = "gen_ai.system_instructions"
	AttrGenAIConversationID     = "gen_ai.conversation.id"

	// LLM / embedding span attributes
	AttrGenAIRequestModel       = "gen_ai.request.model"
	AttrGenAIResponseModel      = "gen_ai.response.model"
	AttrGenAIRequestTemperature = "gen_ai.request.temperature"
	AttrGenAIInputTools         = "gen_ai.input.tools"

	// Token usage
	AttrGenAIUsageInputTokens  = "gen_ai.usage.input_tokens"
	AttrGenAIUsageOutputTokens = "gen_ai.usage.output_tokens"

	// Tool span attributes
	AttrGenAIToolName        = "gen_ai.tool.name"
	AttrGenAIToolDescription = "gen_ai.tool.description"
	AttrGenAIToolCallID      = "gen_ai.tool.call.id"

	// Message content (redacted when AMP_TRACE_CONTENT=false)
	AttrGenAIInputMessages  = "gen_ai.input.messages"
	AttrGenAIOutputMessages = "gen_ai.output.messages"

	// Retriever span attributes (OTel database semantic conventions)
	AttrDBSystemName      = "db.system.name"
	AttrDBCollectionName  = "db.collection.name"
	AttrDBVectorQueryTopK = "db.vector.query.top_k"

	// Layer 2 — OpenLLMetry extension keys
	AttrTraceloopSpanKind     = "traceloop.span.kind"
	AttrTraceloopEntityInput  = "traceloop.entity.input"
	AttrTraceloopEntityOutput = "traceloop.entity.output"
	AttrRerankModel           = "rerank.model"

	// Error badge
	AttrErrorType = "error.type"

	// Evaluation correlation, copied from W3C baggage onto the agent span.
	AttrTaskID  = "task.id"
	AttrTrialID = "trial.id"
)

// gen_ai.operation.name values.
const (
	OperationChat        = "chat"
	OperationEmbeddings  = "embeddings"
	OperationExecuteTool = "execute_tool"
	OperationInvokeAgent = "invoke_agent"
	OperationRerank      = "rerank" // de-facto, not standard OTel
)

// traceloop.span.kind values, for the kinds OTel has no signal for.
const (
	SpanKindWorkflow = "workflow" // rendered as a chain
	SpanKindRerank   = "rerank"
)

// W3C baggage keys that join a trace to the evaluation trial that produced it.
const (
	BaggageTaskID  = "task_id"
	BaggageTrialID = "trial_id"
)

// redacted replaces free text when content tracing is off, so spans keep their
// structure and metadata without carrying prompts or completions.
const redacted = "[redacted]"

// TracerManager handles OTLP exporter setup and provides span helper methods.
type TracerManager struct {
	cfg            Config
	logger         *zap.Logger
	tracerProvider *sdktrace.TracerProvider
	tracer         trace.Tracer
	propagator     propagation.TextMapPropagator
}

// NewTracerManager configures the OTLP/HTTP exporter, BatchSpanProcessor,
// and TracerProvider. The provider is registered as the global default.
func NewTracerManager(cfg Config, logger *zap.Logger) (*TracerManager, error) {
	tp, err := newTracerProvider(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	installGlobalTracing(tp)

	tm := &TracerManager{
		cfg:            cfg,
		logger:         logger.Named("tracer"),
		tracerProvider: tp,
		tracer:         tp.Tracer("amp-go", trace.WithInstrumentationVersion(Version)),
		propagator:     tracePropagator(),
	}

	logger.Info("OTLP tracer configured",
		zap.String("endpoint", cfg.OTLPTracesEndpoint()),
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
//
// One helper per span kind in the contract. Each opens a span, sets the
// request attributes, runs fn with the span's context, then writes the
// response attributes from fn's result. A non-nil error from fn marks the span
// as failed (see MarkError) and is returned unchanged.

// Message represents a chat message in the OpenAI-compatible format.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction names the tool and carries its JSON-encoded arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition describes a tool made available to an agent or LLM call.
type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// AgentRequest describes one agent invocation.
type AgentRequest struct {
	Name               string           // gen_ai.agent.name; defaults to Config.AgentName
	Description        string           // gen_ai.agent.description
	Framework          string           // gen_ai.system — framework chip
	Model              string           // gen_ai.request.model
	SystemInstructions string           // gen_ai.system_instructions
	ConversationID     string           // gen_ai.conversation.id
	Messages           []Message        // gen_ai.input.messages
	Tools              []ToolDefinition // gen_ai.agent.tools
}

// AgentResult is returned by the function passed to AgentSpan.
type AgentResult struct {
	Messages     []Message // gen_ai.output.messages
	InputTokens  int
	OutputTokens int
}

// AgentSpan creates the root span for one agent invocation
// (gen_ai.operation.name = invoke_agent). All child spans must be created
// from the context passed to fn.
func (tm *TracerManager) AgentSpan(
	ctx context.Context,
	req AgentRequest,
	fn func(context.Context) (AgentResult, error),
) (AgentResult, error) {
	ctx, span := tm.tracer.Start(ctx, OperationInvokeAgent, trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	name := req.Name
	if name == "" {
		name = tm.cfg.AgentName
	}

	span.SetAttributes(attribute.String(AttrGenAIOperationName, OperationInvokeAgent))
	setString(span, AttrGenAISystem, req.Framework)
	setString(span, AttrGenAIAgentName, name)
	setString(span, AttrGenAIAgentDescription, req.Description)
	setString(span, AttrGenAIRequestModel, req.Model)
	setString(span, AttrGenAIConversationID, req.ConversationID)
	if req.SystemInstructions != "" {
		span.SetAttributes(attribute.String(AttrGenAISystemInstructions, tm.content(req.SystemInstructions)))
	}
	if len(req.Messages) > 0 {
		span.SetAttributes(attribute.String(AttrGenAIInputMessages, tm.messages(req.Messages)))
	}
	if len(req.Tools) > 0 {
		span.SetAttributes(attribute.String(AttrGenAIAgentTools, jsonString(req.Tools)))
	}

	// The observer reads evaluation correlation from the root span.
	bag := baggage.FromContext(ctx)
	setString(span, AttrTaskID, bag.Member(BaggageTaskID).Value())
	setString(span, AttrTrialID, bag.Member(BaggageTrialID).Value())

	result, err := fn(ctx)
	if err != nil {
		MarkError(span, err)
	}

	if len(result.Messages) > 0 {
		span.SetAttributes(attribute.String(AttrGenAIOutputMessages, tm.messages(result.Messages)))
	}
	span.SetAttributes(
		attribute.Int(AttrGenAIUsageInputTokens, result.InputTokens),
		attribute.Int(AttrGenAIUsageOutputTokens, result.OutputTokens),
	)

	return result, err
}

// ChainSpan creates a workflow / pipeline step. OTel has no key for this kind,
// so the observer reads the Layer-2 traceloop.span.kind.
func (tm *TracerManager) ChainSpan(
	ctx context.Context,
	name string,
	input any,
	fn func(context.Context) (any, error),
) (any, error) {
	ctx, span := tm.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	span.SetAttributes(
		attribute.String(AttrTraceloopSpanKind, SpanKindWorkflow),
		attribute.String(AttrTraceloopEntityInput, jsonString(map[string]any{"input": tm.value(input)})),
	)

	output, err := fn(ctx)
	if err != nil {
		MarkError(span, err)
	}

	span.SetAttributes(
		attribute.String(AttrTraceloopEntityOutput, jsonString(map[string]any{"output": tm.value(output)})),
	)

	return output, err
}

// LLMRequest describes one LLM chat call.
type LLMRequest struct {
	Model       string           // gen_ai.request.model, e.g. "gpt-4o"
	Provider    string           // gen_ai.system, e.g. "openai", "ollama"
	Temperature *float64         // gen_ai.request.temperature; nil omits it
	Messages    []Message        // gen_ai.input.messages
	Tools       []ToolDefinition // gen_ai.input.tools
}

// LLMResult is returned by the function passed to LLMSpan.
type LLMResult struct {
	Content          string
	ToolCalls        []ToolCall // tool calls the model asked for, if any
	PromptTokens     int
	CompletionTokens int
	FinishReason     string
	Model            string // actual model used (may differ from requested)
}

// Float64 returns a pointer to v, for optional fields such as
// LLMRequest.Temperature.
func Float64(v float64) *float64 { return &v }

// LLMSpan creates an LLM chat span (gen_ai.operation.name = chat). LLM
// evaluators need the input and output messages, so those are always written
// — redacted when AMP_TRACE_CONTENT is off.
func (tm *TracerManager) LLMSpan(
	ctx context.Context,
	req LLMRequest,
	fn func(context.Context) (LLMResult, error),
) (LLMResult, error) {
	ctx, span := tm.tracer.Start(ctx, OperationChat, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	span.SetAttributes(
		attribute.String(AttrGenAIOperationName, OperationChat),
		attribute.String(AttrGenAISystem, req.Provider),
		attribute.String(AttrGenAIRequestModel, req.Model),
		attribute.String(AttrGenAIInputMessages, tm.messages(req.Messages)),
	)
	if req.Temperature != nil {
		span.SetAttributes(attribute.Float64(AttrGenAIRequestTemperature, *req.Temperature))
	}
	if len(req.Tools) > 0 {
		span.SetAttributes(attribute.String(AttrGenAIInputTools, jsonString(req.Tools)))
	}

	result, err := fn(ctx)
	if err != nil {
		MarkError(span, err)
		return result, err
	}

	setString(span, AttrGenAIResponseModel, result.Model)
	if result.Content != "" || len(result.ToolCalls) > 0 {
		out := []Message{{Role: "assistant", Content: result.Content, ToolCalls: result.ToolCalls}}
		span.SetAttributes(attribute.String(AttrGenAIOutputMessages, tm.messages(out)))
	}
	span.SetAttributes(
		attribute.Int(AttrGenAIUsageInputTokens, result.PromptTokens),
		attribute.Int(AttrGenAIUsageOutputTokens, result.CompletionTokens),
	)

	return result, nil
}

// EmbeddingRequest describes one embedding call.
type EmbeddingRequest struct {
	Model    string   // gen_ai.request.model
	Provider string   // gen_ai.system
	Texts    []string // gen_ai.prompt.{i}.content
}

// EmbeddingResult is returned by the function passed to EmbeddingSpan.
type EmbeddingResult struct {
	Model       string // actual model used
	InputTokens int
}

// EmbeddingSpan creates an embedding span
// (gen_ai.operation.name = embeddings).
func (tm *TracerManager) EmbeddingSpan(
	ctx context.Context,
	req EmbeddingRequest,
	fn func(context.Context) (EmbeddingResult, error),
) (EmbeddingResult, error) {
	ctx, span := tm.tracer.Start(ctx, OperationEmbeddings, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	span.SetAttributes(
		attribute.String(AttrGenAIOperationName, OperationEmbeddings),
		attribute.String(AttrGenAISystem, req.Provider),
		attribute.String(AttrGenAIRequestModel, req.Model),
	)
	for i, text := range req.Texts {
		// Indexed embedded text. The OTel key here is not fully settled; this
		// is the de-facto form the observer reads.
		span.SetAttributes(attribute.String(fmt.Sprintf("gen_ai.prompt.%d.content", i), tm.content(text)))
	}

	result, err := fn(ctx)
	if err != nil {
		MarkError(span, err)
		return result, err
	}

	setString(span, AttrGenAIResponseModel, result.Model)
	span.SetAttributes(attribute.Int(AttrGenAIUsageInputTokens, result.InputTokens))

	return result, nil
}

// RetrieverRequest describes one vector-DB retrieval.
type RetrieverRequest struct {
	VectorDB   string // db.system.name, e.g. "pinecone", "chroma", "qdrant", "pgvector"
	Collection string // db.collection.name
	TopK       int    // db.vector.query.top_k
}

// RetrieverSpan creates a retrieval span. The observer keys kind = retriever
// off db.system.name. Retrieved documents are not extracted in v1, so they are
// not recorded.
func (tm *TracerManager) RetrieverSpan(
	ctx context.Context,
	req RetrieverRequest,
	fn func(context.Context) error,
) error {
	ctx, span := tm.tracer.Start(ctx, "vector_search", trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	span.SetAttributes(attribute.String(AttrDBSystemName, req.VectorDB))
	setString(span, AttrDBCollectionName, req.Collection)
	if req.TopK > 0 {
		span.SetAttributes(attribute.Int(AttrDBVectorQueryTopK, req.TopK))
	}

	err := fn(ctx)
	if err != nil {
		MarkError(span, err)
	}
	return err
}

// RerankRequest describes one reranking step.
type RerankRequest struct {
	Model          string // rerank.model and gen_ai.request.model
	Query          string
	CandidateCount int
}

// RerankSpan creates a reranking span. Rerank is recognized as a kind only —
// it gets the rerank icon, no data card.
func (tm *TracerManager) RerankSpan(
	ctx context.Context,
	req RerankRequest,
	fn func(context.Context) error,
) error {
	ctx, span := tm.tracer.Start(ctx, OperationRerank, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()

	span.SetAttributes(
		attribute.String(AttrTraceloopSpanKind, SpanKindRerank),
		attribute.String(AttrGenAIOperationName, OperationRerank),
		attribute.String(AttrRerankModel, req.Model),
		attribute.String(AttrGenAIRequestModel, req.Model),
		attribute.String(AttrTraceloopEntityInput, jsonString(map[string]any{
			"query":           tm.content(req.Query),
			"candidate_count": req.CandidateCount,
		})),
	)

	err := fn(ctx)
	if err != nil {
		MarkError(span, err)
	}
	return err
}

// ToolRequest describes one tool / function call.
type ToolRequest struct {
	Name        string // gen_ai.tool.name
	Description string // gen_ai.tool.description
	CallID      string // gen_ai.tool.call.id — matches the ToolCall.ID that asked for it
	Arguments   any    // traceloop.entity.input
}

// ToolSpan creates a tool span (gen_ai.operation.name = execute_tool). OTel
// has no stable key for tool I/O, so arguments and result go in the Layer-2
// traceloop.entity.* keys.
func (tm *TracerManager) ToolSpan(
	ctx context.Context,
	req ToolRequest,
	fn func(context.Context) (any, error),
) (any, error) {
	ctx, span := tm.tracer.Start(ctx, OperationExecuteTool, trace.WithSpanKind(trace.SpanKindInternal))
	defer span.End()

	span.SetAttributes(
		attribute.String(AttrGenAIOperationName, OperationExecuteTool),
		attribute.String(AttrGenAIToolName, req.Name),
	)
	setString(span, AttrGenAIToolDescription, req.Description)
	setString(span, AttrGenAIToolCallID, req.CallID)
	if req.Arguments != nil {
		span.SetAttributes(attribute.String(AttrTraceloopEntityInput, jsonString(tm.value(req.Arguments))))
	}

	result, err := fn(ctx)
	if err != nil {
		MarkError(span, err)
		return result, err
	}

	span.SetAttributes(
		attribute.String(AttrTraceloopEntityOutput, jsonString(map[string]any{"result": tm.value(result)})),
	)

	return result, nil
}

// MarkError flags a span as failed: span status Error plus error.type. The
// observer turns this into an error badge on the span and bumps the trace's
// error count. Works on any span kind.
func MarkError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	span.SetAttributes(attribute.String(AttrErrorType, fmt.Sprintf("%T", err)))
}

// WithEvaluationBaggage attaches W3C baggage so a trace can be joined to the
// evaluation trial that produced it. Optional; only relevant when the agent is
// driven by an evaluation run. Empty IDs are skipped.
func WithEvaluationBaggage(ctx context.Context, taskID, trialID string) context.Context {
	bag := baggage.FromContext(ctx)
	for _, kv := range [][2]string{{BaggageTaskID, taskID}, {BaggageTrialID, trialID}} {
		if kv[1] == "" {
			continue
		}
		member, err := baggage.NewMemberRaw(kv[0], kv[1])
		if err != nil {
			continue
		}
		if next, err := bag.SetMember(member); err == nil {
			bag = next
		}
	}
	return baggage.ContextWithBaggage(ctx, bag)
}

// InjectTraceparent injects W3C traceparent/tracestate (and baggage) headers
// into an HTTP request for distributed trace correlation.
func (tm *TracerManager) InjectTraceparent(ctx context.Context, req *http.Request) {
	tm.propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))
}

// --- Content capture ---
//
// On the manual path the agent controls span content itself. AMP_TRACE_CONTENT
// suppresses prompt/completion text without dropping the spans: free text is
// replaced with "[redacted]" and structure (roles, tool calls, keys) is kept.

func (tm *TracerManager) content(text string) string {
	if tm.cfg.TraceContent {
		return text
	}
	return redacted
}

func (tm *TracerManager) messages(msgs []Message) string {
	if msgs == nil {
		msgs = []Message{}
	}
	if tm.cfg.TraceContent {
		return jsonString(msgs)
	}
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		if m.Content != "" {
			m.Content = redacted
		}
		if len(m.ToolCalls) > 0 {
			// A tool call's arguments string is content too.
			calls := make([]ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				tc.Function.Arguments = redacted
				calls[j] = tc
			}
			m.ToolCalls = calls
		}
		out[i] = m
	}
	return jsonString(out)
}

// value redacts string content anywhere inside an arbitrary value when content
// tracing is off, preserving structure. Used for the free-form
// traceloop.entity.* payloads.
func (tm *TracerManager) value(v any) any {
	if tm.cfg.TraceContent || v == nil {
		return v
	}
	b, err := json.Marshal(v)
	if err != nil {
		return redacted
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return redacted
	}
	return redactValue(generic)
}

func redactValue(v any) any {
	switch t := v.(type) {
	case string:
		return redacted
	case []any:
		for i := range t {
			t[i] = redactValue(t[i])
		}
	case map[string]any:
		for k := range t {
			t[k] = redactValue(t[k])
		}
	}
	return v
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// setString sets a string attribute only when it has a value, so optional
// contract keys are omitted rather than exported empty.
func setString(span trace.Span, key, value string) {
	if value != "" {
		span.SetAttributes(attribute.String(key, value))
	}
}
