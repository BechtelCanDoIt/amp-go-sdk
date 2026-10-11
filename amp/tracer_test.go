package amp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// newTestTracerManager creates a TracerManager with an in-memory exporter
// for verifying span attributes without a real OTLP collector.
func newTestTracerManager(t *testing.T, cfg Config) (*TracerManager, *tracetest.InMemoryExporter) {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter), // Synchronous for tests
	)

	logger, _ := zap.NewDevelopment()

	tm := &TracerManager{
		cfg:            cfg,
		logger:         logger.Named("tracer-test"),
		tracerProvider: tp,
		tracer:         tp.Tracer("amp-go-test"),
		propagator:     tracePropagator(),
	}

	return tm, exporter
}

func findAttr(attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestAgentSpan_Attributes(t *testing.T) {
	cfg := Config{
		AgentName:    "test-agent",
		TraceContent: true,
	}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	_, err := tm.AgentSpan(context.Background(), AgentRequest{
		Description:        "Answers questions",
		Framework:          "custom-framework",
		Model:              "gpt-4o",
		SystemInstructions: "Be concise.",
		ConversationID:     "conv-123",
		Messages:           []Message{{Role: "user", Content: "Hello"}},
		Tools:              []ToolDefinition{{Name: "search_docs", Description: "Searches docs"}},
	}, func(ctx context.Context) (AgentResult, error) {
		// Verify we're inside a span.
		span := trace.SpanFromContext(ctx)
		if !span.SpanContext().IsValid() {
			t.Error("expected valid span context")
		}
		return AgentResult{
			Messages:     []Message{{Role: "assistant", Content: "Hi"}},
			InputTokens:  12,
			OutputTokens: 34,
		}, nil
	})

	if err != nil {
		t.Fatalf("AgentSpan() error = %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	s := spans[0]
	attrs := s.Attributes

	if s.Name != "invoke_agent" {
		t.Errorf("span name = %q, want %q", s.Name, "invoke_agent")
	}
	if s.SpanKind != trace.SpanKindInternal {
		t.Errorf("span kind = %v, want internal", s.SpanKind)
	}

	// Verify the contract's agent span attributes.
	assertAttr(t, attrs, AttrGenAIOperationName, "invoke_agent")
	assertAttr(t, attrs, AttrGenAIAgentName, "test-agent") // falls back to Config.AgentName
	assertAttr(t, attrs, AttrGenAIAgentDescription, "Answers questions")
	assertAttr(t, attrs, AttrGenAISystem, "custom-framework")
	assertAttr(t, attrs, AttrGenAIRequestModel, "gpt-4o")
	assertAttr(t, attrs, AttrGenAISystemInstructions, "Be concise.")
	assertAttr(t, attrs, AttrGenAIConversationID, "conv-123")
	assertAttr(t, attrs, AttrGenAIInputMessages, `[{"role":"user","content":"Hello"}]`)
	assertAttr(t, attrs, AttrGenAIOutputMessages, `[{"role":"assistant","content":"Hi"}]`)
	assertAttr(t, attrs, AttrGenAIAgentTools, `[{"name":"search_docs","description":"Searches docs"}]`)
	assertAttrInt(t, attrs, AttrGenAIUsageInputTokens, 12)
	assertAttrInt(t, attrs, AttrGenAIUsageOutputTokens, 34)

	// The kind comes from gen_ai.operation.name, not the traceloop extension.
	if _, ok := findAttr(attrs, AttrTraceloopSpanKind); ok {
		t.Error("agent span should not set traceloop.span.kind")
	}
}

func TestAgentSpan_ContentRedacted(t *testing.T) {
	cfg := Config{
		AgentName:    "test-agent",
		TraceContent: false, // Disabled
	}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	tm.AgentSpan(context.Background(), AgentRequest{
		SystemInstructions: "Be concise.",
		Messages:           []Message{{Role: "user", Content: "Hello"}},
	}, func(ctx context.Context) (AgentResult, error) {
		return AgentResult{Messages: []Message{{Role: "assistant", Content: "Hi"}}}, nil
	})

	attrs := exporter.GetSpans()[0].Attributes

	// Content is redacted, structure is kept.
	assertAttr(t, attrs, AttrGenAISystemInstructions, "[redacted]")
	assertAttr(t, attrs, AttrGenAIInputMessages, `[{"role":"user","content":"[redacted]"}]`)
	assertAttr(t, attrs, AttrGenAIOutputMessages, `[{"role":"assistant","content":"[redacted]"}]`)
}

func TestAgentSpan_EvaluationBaggage(t *testing.T) {
	tm, exporter := newTestTracerManager(t, Config{AgentName: "test"})
	defer tm.Shutdown(context.Background())

	ctx := WithEvaluationBaggage(context.Background(), "task-1", "trial-9")
	tm.AgentSpan(ctx, AgentRequest{}, func(ctx context.Context) (AgentResult, error) {
		// Baggage propagates to outbound calls made inside the span.
		req, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
		tm.InjectTraceparent(ctx, req)
		got := req.Header.Get("baggage")
		if !strings.Contains(got, "task_id=task-1") || !strings.Contains(got, "trial_id=trial-9") {
			t.Errorf("baggage header = %q, want task_id and trial_id", got)
		}
		if req.Header.Get("traceparent") == "" {
			t.Error("expected traceparent header")
		}
		return AgentResult{}, nil
	})

	attrs := exporter.GetSpans()[0].Attributes
	assertAttr(t, attrs, AttrTaskID, "task-1")
	assertAttr(t, attrs, AttrTrialID, "trial-9")
}

func TestWithEvaluationBaggage_SkipsEmpty(t *testing.T) {
	tm, exporter := newTestTracerManager(t, Config{AgentName: "test"})
	defer tm.Shutdown(context.Background())

	ctx := WithEvaluationBaggage(context.Background(), "", "")
	tm.AgentSpan(ctx, AgentRequest{}, func(ctx context.Context) (AgentResult, error) {
		return AgentResult{}, nil
	})

	attrs := exporter.GetSpans()[0].Attributes
	if _, ok := findAttr(attrs, AttrTaskID); ok {
		t.Error("task.id should be absent without baggage")
	}
	if _, ok := findAttr(attrs, AttrTrialID); ok {
		t.Error("trial.id should be absent without baggage")
	}
}

func TestLLMSpan_Attributes(t *testing.T) {
	cfg := Config{
		AgentName:    "test-agent",
		TraceContent: true,
	}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	result, err := tm.LLMSpan(context.Background(), LLMRequest{
		Model:       "gpt-4o",
		Provider:    "openai",
		Temperature: Float64(0.7),
		Messages:    []Message{{Role: "user", Content: "Hello"}},
		Tools:       []ToolDefinition{{Name: "word_count"}},
	}, func(ctx context.Context) (LLMResult, error) {
		return LLMResult{
			Content:          "Hi there!",
			PromptTokens:     42,
			CompletionTokens: 87,
			FinishReason:     "stop",
			Model:            "gpt-4o-2024-05-13",
		}, nil
	})

	if err != nil {
		t.Fatalf("LLMSpan() error = %v", err)
	}
	if result.Content != "Hi there!" {
		t.Errorf("result.Content = %q, want %q", result.Content, "Hi there!")
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	if spans[0].Name != "chat" {
		t.Errorf("span name = %q, want %q", spans[0].Name, "chat")
	}
	if spans[0].SpanKind != trace.SpanKindClient {
		t.Errorf("span kind = %v, want client", spans[0].SpanKind)
	}

	attrs := spans[0].Attributes

	// Verify the contract's LLM span attributes.
	assertAttr(t, attrs, AttrGenAIOperationName, "chat")
	assertAttr(t, attrs, AttrGenAISystem, "openai")
	assertAttr(t, attrs, AttrGenAIRequestModel, "gpt-4o")
	assertAttr(t, attrs, AttrGenAIResponseModel, "gpt-4o-2024-05-13")
	assertAttr(t, attrs, AttrGenAIInputMessages, `[{"role":"user","content":"Hello"}]`)
	assertAttr(t, attrs, AttrGenAIOutputMessages, `[{"role":"assistant","content":"Hi there!"}]`)
	assertAttr(t, attrs, AttrGenAIInputTools, `[{"name":"word_count"}]`)

	// Token usage.
	assertAttrInt(t, attrs, AttrGenAIUsageInputTokens, 42)
	assertAttrInt(t, attrs, AttrGenAIUsageOutputTokens, 87)

	// Temperature.
	v, ok := findAttr(attrs, AttrGenAIRequestTemperature)
	if !ok {
		t.Error("missing gen_ai.request.temperature")
	}
	if v.AsFloat64() != 0.7 {
		t.Errorf("temperature = %f, want 0.7", v.AsFloat64())
	}
}

func TestLLMSpan_OptionalAttributesOmitted(t *testing.T) {
	cfg := Config{AgentName: "test", TraceContent: true}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	tm.LLMSpan(context.Background(), LLMRequest{
		Model:    "gpt-4o",
		Provider: "openai",
	}, func(ctx context.Context) (LLMResult, error) {
		return LLMResult{Content: "response"}, nil
	})

	attrs := exporter.GetSpans()[0].Attributes
	for _, key := range []string{AttrGenAIRequestTemperature, AttrGenAIResponseModel, AttrGenAIInputTools} {
		if _, ok := findAttr(attrs, key); ok {
			t.Errorf("%s should be absent when not provided", key)
		}
	}
}

func TestLLMSpan_ToolCallsRedacted(t *testing.T) {
	cfg := Config{AgentName: "test", TraceContent: false}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	call := ToolCall{
		ID:       "call_1",
		Type:     "function",
		Function: ToolCallFunction{Name: "word_count", Arguments: `{"text":"secret"}`},
	}
	tm.LLMSpan(context.Background(), LLMRequest{
		Model:    "gpt-4o",
		Provider: "openai",
		Messages: []Message{{Role: "user", Content: "secret"}},
	}, func(ctx context.Context) (LLMResult, error) {
		return LLMResult{ToolCalls: []ToolCall{call}}, nil
	})

	attrs := exporter.GetSpans()[0].Attributes
	out, _ := findAttr(attrs, AttrGenAIOutputMessages)

	var msgs []Message
	if err := json.Unmarshal([]byte(out.AsString()), &msgs); err != nil {
		t.Fatalf("output messages are not JSON: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("unexpected output messages: %s", out.AsString())
	}
	got := msgs[0].ToolCalls[0]
	if got.ID != "call_1" || got.Function.Name != "word_count" {
		t.Errorf("tool call structure not preserved: %+v", got)
	}
	if got.Function.Arguments != "[redacted]" {
		t.Errorf("tool call arguments = %q, want redacted", got.Function.Arguments)
	}
	if strings.Contains(out.AsString(), "secret") {
		t.Error("output messages leak content with TraceContent=false")
	}
	if call.Function.Arguments != `{"text":"secret"}` {
		t.Error("redaction must not mutate the caller's tool call")
	}
}

func TestToolSpan_Attributes(t *testing.T) {
	cfg := Config{AgentName: "test", TraceContent: true}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	result, err := tm.ToolSpan(context.Background(), ToolRequest{
		Name:        "weather_lookup",
		Description: "Looks up the weather",
		CallID:      "call_42",
		Arguments:   map[string]string{"query": "weather in Nashville"},
	}, func(ctx context.Context) (any, error) {
		return map[string]string{"temp": "72F"}, nil
	})

	if err != nil {
		t.Fatalf("ToolSpan() error = %v", err)
	}
	if result == nil {
		t.Error("expected non-nil result")
	}

	spans := exporter.GetSpans()
	if spans[0].Name != "execute_tool" {
		t.Errorf("span name = %q, want %q", spans[0].Name, "execute_tool")
	}
	attrs := spans[0].Attributes

	assertAttr(t, attrs, AttrGenAIOperationName, "execute_tool")
	assertAttr(t, attrs, AttrGenAIToolName, "weather_lookup")
	assertAttr(t, attrs, AttrGenAIToolDescription, "Looks up the weather")
	assertAttr(t, attrs, AttrGenAIToolCallID, "call_42")
	assertAttr(t, attrs, AttrTraceloopEntityInput, `{"query":"weather in Nashville"}`)
	assertAttr(t, attrs, AttrTraceloopEntityOutput, `{"result":{"temp":"72F"}}`)
}

func TestToolSpan_ContentRedacted(t *testing.T) {
	cfg := Config{AgentName: "test", TraceContent: false}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	tm.ToolSpan(context.Background(), ToolRequest{
		Name:      "weather_lookup",
		Arguments: map[string]any{"query": "weather in Nashville", "days": 3},
	}, func(ctx context.Context) (any, error) {
		return []string{"72F"}, nil
	})

	attrs := exporter.GetSpans()[0].Attributes
	assertAttr(t, attrs, AttrTraceloopEntityInput, `{"days":3,"query":"[redacted]"}`)
	assertAttr(t, attrs, AttrTraceloopEntityOutput, `{"result":["[redacted]"]}`)
}

func TestChainSpan_Attributes(t *testing.T) {
	cfg := Config{AgentName: "test", TraceContent: true}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	out, err := tm.ChainSpan(context.Background(), "rag-pipeline", "question",
		func(ctx context.Context) (any, error) { return "answer", nil },
	)
	if err != nil || out != "answer" {
		t.Fatalf("ChainSpan() = %v, %v", out, err)
	}

	s := exporter.GetSpans()[0]
	if s.Name != "rag-pipeline" {
		t.Errorf("span name = %q, want %q", s.Name, "rag-pipeline")
	}
	assertAttr(t, s.Attributes, AttrTraceloopSpanKind, "workflow")
	assertAttr(t, s.Attributes, AttrTraceloopEntityInput, `{"input":"question"}`)
	assertAttr(t, s.Attributes, AttrTraceloopEntityOutput, `{"output":"answer"}`)
}

func TestEmbeddingSpan_Attributes(t *testing.T) {
	cfg := Config{AgentName: "test", TraceContent: true}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	_, err := tm.EmbeddingSpan(context.Background(), EmbeddingRequest{
		Model:    "text-embedding-3-small",
		Provider: "openai",
		Texts:    []string{"first", "second"},
	}, func(ctx context.Context) (EmbeddingResult, error) {
		return EmbeddingResult{Model: "text-embedding-3-small-v2", InputTokens: 5}, nil
	})
	if err != nil {
		t.Fatalf("EmbeddingSpan() error = %v", err)
	}

	s := exporter.GetSpans()[0]
	if s.Name != "embeddings" {
		t.Errorf("span name = %q, want %q", s.Name, "embeddings")
	}
	assertAttr(t, s.Attributes, AttrGenAIOperationName, "embeddings")
	assertAttr(t, s.Attributes, AttrGenAISystem, "openai")
	assertAttr(t, s.Attributes, AttrGenAIRequestModel, "text-embedding-3-small")
	assertAttr(t, s.Attributes, AttrGenAIResponseModel, "text-embedding-3-small-v2")
	assertAttr(t, s.Attributes, "gen_ai.prompt.0.content", "first")
	assertAttr(t, s.Attributes, "gen_ai.prompt.1.content", "second")
	assertAttrInt(t, s.Attributes, AttrGenAIUsageInputTokens, 5)
}

func TestRetrieverSpan_Attributes(t *testing.T) {
	tm, exporter := newTestTracerManager(t, Config{AgentName: "test"})
	defer tm.Shutdown(context.Background())

	err := tm.RetrieverSpan(context.Background(), RetrieverRequest{
		VectorDB:   "chroma",
		Collection: "kb",
		TopK:       3,
	}, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("RetrieverSpan() error = %v", err)
	}

	s := exporter.GetSpans()[0]
	if s.Name != "vector_search" {
		t.Errorf("span name = %q, want %q", s.Name, "vector_search")
	}
	assertAttr(t, s.Attributes, AttrDBSystemName, "chroma")
	assertAttr(t, s.Attributes, AttrDBCollectionName, "kb")
	assertAttrInt(t, s.Attributes, AttrDBVectorQueryTopK, 3)
}

func TestRerankSpan_Attributes(t *testing.T) {
	cfg := Config{AgentName: "test", TraceContent: true}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	err := tm.RerankSpan(context.Background(), RerankRequest{
		Model:          "rerank-english-v3.0",
		Query:          "question",
		CandidateCount: 3,
	}, func(ctx context.Context) error { return nil })
	if err != nil {
		t.Fatalf("RerankSpan() error = %v", err)
	}

	s := exporter.GetSpans()[0]
	if s.Name != "rerank" {
		t.Errorf("span name = %q, want %q", s.Name, "rerank")
	}
	assertAttr(t, s.Attributes, AttrTraceloopSpanKind, "rerank")
	assertAttr(t, s.Attributes, AttrGenAIOperationName, "rerank")
	assertAttr(t, s.Attributes, AttrRerankModel, "rerank-english-v3.0")
	assertAttr(t, s.Attributes, AttrGenAIRequestModel, "rerank-english-v3.0")
	assertAttr(t, s.Attributes, AttrTraceloopEntityInput, `{"candidate_count":3,"query":"question"}`)
}

func TestNestedSpans_AgentContainsLLM(t *testing.T) {
	cfg := Config{AgentName: "test-agent", TraceContent: false}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	tm.AgentSpan(context.Background(), AgentRequest{ConversationID: "conv-1"},
		func(agentCtx context.Context) (AgentResult, error) {
			_, err := tm.LLMSpan(agentCtx, LLMRequest{
				Model:    "gpt-4o",
				Provider: "openai",
			}, func(llmCtx context.Context) (LLMResult, error) {
				return LLMResult{Content: "response", PromptTokens: 10, CompletionTokens: 20}, nil
			})
			return AgentResult{}, err
		},
	)

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans (agent + llm), got %d", len(spans))
	}

	// In-memory exporter records spans in end-order: LLM ends first.
	llmSpan := spans[0]
	agentSpan := spans[1]

	// Verify parent-child relationship.
	if llmSpan.Parent.TraceID() != agentSpan.SpanContext.TraceID() {
		t.Error("LLM span should share trace ID with agent span")
	}
	if llmSpan.Parent.SpanID() != agentSpan.SpanContext.SpanID() {
		t.Error("LLM span parent should be the agent span")
	}

	// Verify types.
	assertAttr(t, llmSpan.Attributes, AttrGenAIOperationName, "chat")
	assertAttr(t, agentSpan.Attributes, AttrGenAIOperationName, "invoke_agent")
}

func TestNestedSpans_AgentContainsLLMAndTool(t *testing.T) {
	cfg := Config{AgentName: "test-agent", TraceContent: false}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	tm.AgentSpan(context.Background(), AgentRequest{ConversationID: "conv-2"},
		func(agentCtx context.Context) (AgentResult, error) {
			// Tool call first.
			_, err := tm.ToolSpan(agentCtx, ToolRequest{Name: "search_docs"},
				func(toolCtx context.Context) (any, error) {
					return "doc content", nil
				},
			)
			if err != nil {
				return AgentResult{}, err
			}

			// Then LLM call.
			_, err = tm.LLMSpan(agentCtx, LLMRequest{
				Model:    "gpt-4o",
				Provider: "openai",
			}, func(llmCtx context.Context) (LLMResult, error) {
				return LLMResult{Content: "answer"}, nil
			})
			return AgentResult{}, err
		},
	)

	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans (tool + llm + agent), got %d", len(spans))
	}

	// All should share the same trace ID.
	traceID := spans[0].SpanContext.TraceID()
	for i, s := range spans {
		if s.SpanContext.TraceID() != traceID {
			t.Errorf("span[%d] has different trace ID", i)
		}
	}
}

func TestAgentSpan_PropagatesError(t *testing.T) {
	cfg := Config{AgentName: "test", TraceContent: false}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	testErr := fmt.Errorf("LLM backend timeout")
	_, err := tm.AgentSpan(context.Background(), AgentRequest{},
		func(ctx context.Context) (AgentResult, error) {
			return AgentResult{}, testErr
		},
	)

	if err != testErr {
		t.Errorf("AgentSpan() error = %v, want %v", err, testErr)
	}

	s := exporter.GetSpans()[0]
	if len(s.Events) == 0 {
		t.Error("expected error event recorded on span")
	}
	// The contract's error badge: span status Error plus error.type.
	if s.Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", s.Status.Code)
	}
	if s.Status.Description != "LLM backend timeout" {
		t.Errorf("status description = %q", s.Status.Description)
	}
	if _, ok := findAttr(s.Attributes, AttrErrorType); !ok {
		t.Error("missing error.type attribute")
	}
}

func TestSpanHelpers_MarkErrorOnFailure(t *testing.T) {
	tm, exporter := newTestTracerManager(t, Config{AgentName: "test"})
	defer tm.Shutdown(context.Background())

	boom := fmt.Errorf("boom")
	ctx := context.Background()

	tm.LLMSpan(ctx, LLMRequest{}, func(context.Context) (LLMResult, error) { return LLMResult{}, boom })
	tm.ToolSpan(ctx, ToolRequest{Name: "t"}, func(context.Context) (any, error) { return nil, boom })
	tm.ChainSpan(ctx, "c", nil, func(context.Context) (any, error) { return nil, boom })
	tm.EmbeddingSpan(ctx, EmbeddingRequest{}, func(context.Context) (EmbeddingResult, error) { return EmbeddingResult{}, boom })
	tm.RetrieverSpan(ctx, RetrieverRequest{VectorDB: "chroma"}, func(context.Context) error { return boom })
	tm.RerankSpan(ctx, RerankRequest{}, func(context.Context) error { return boom })

	spans := exporter.GetSpans()
	if len(spans) != 6 {
		t.Fatalf("expected 6 spans, got %d", len(spans))
	}
	for _, s := range spans {
		if s.Status.Code != codes.Error {
			t.Errorf("span %q status = %v, want Error", s.Name, s.Status.Code)
		}
	}
}

func TestMarkError_NilIsNoOp(t *testing.T) {
	tm, exporter := newTestTracerManager(t, Config{AgentName: "test"})
	defer tm.Shutdown(context.Background())

	_, span := tm.Tracer().Start(context.Background(), "custom")
	MarkError(span, nil)
	span.End()

	s := exporter.GetSpans()[0]
	if s.Status.Code == codes.Error {
		t.Error("MarkError(nil) must not flag the span")
	}
	if _, ok := findAttr(s.Attributes, AttrErrorType); ok {
		t.Error("MarkError(nil) must not set error.type")
	}
}

func TestMarkError_CustomSpan(t *testing.T) {
	// MarkError is also for spans the agent creates itself with Tracer().
	tm, exporter := newTestTracerManager(t, Config{AgentName: "test"})
	defer tm.Shutdown(context.Background())

	_, span := tm.Tracer().Start(context.Background(), "custom")
	MarkError(span, fmt.Errorf("custom failure"))
	span.End()

	s := exporter.GetSpans()[0]
	if s.Status.Code != codes.Error || s.Status.Description != "custom failure" {
		t.Errorf("status = %v %q, want Error %q", s.Status.Code, s.Status.Description, "custom failure")
	}
	assertAttr(t, s.Attributes, AttrErrorType, "*errors.errorString")
}

func TestToolSpan_UnserializablePayload(t *testing.T) {
	// A payload JSON cannot encode must neither panic nor leak with content off.
	tm, exporter := newTestTracerManager(t, Config{AgentName: "test", TraceContent: false})
	defer tm.Shutdown(context.Background())

	_, err := tm.ToolSpan(context.Background(), ToolRequest{Name: "t", Arguments: make(chan int)},
		func(ctx context.Context) (any, error) { return nil, nil },
	)
	if err != nil {
		t.Fatalf("ToolSpan() error = %v", err)
	}

	attrs := exporter.GetSpans()[0].Attributes
	assertAttr(t, attrs, AttrTraceloopEntityInput, `"[redacted]"`)
	assertAttr(t, attrs, AttrTraceloopEntityOutput, `{"result":null}`)
}

func TestLLMSpan_NoMessagesStillWritesInput(t *testing.T) {
	// gen_ai.input.messages is required on llm spans, so it is always present.
	tm, exporter := newTestTracerManager(t, Config{AgentName: "test", TraceContent: true})
	defer tm.Shutdown(context.Background())

	tm.LLMSpan(context.Background(), LLMRequest{Model: "gpt-4o", Provider: "openai"},
		func(ctx context.Context) (LLMResult, error) { return LLMResult{}, nil },
	)

	attrs := exporter.GetSpans()[0].Attributes
	assertAttr(t, attrs, AttrGenAIInputMessages, `[]`)
	if _, ok := findAttr(attrs, AttrGenAIOutputMessages); ok {
		t.Error("gen_ai.output.messages should be absent when the model returned nothing")
	}
}

func TestAgentSpan_ExplicitNameOverridesConfig(t *testing.T) {
	tm, exporter := newTestTracerManager(t, Config{AgentName: "from-config"})
	defer tm.Shutdown(context.Background())

	tm.AgentSpan(context.Background(), AgentRequest{Name: "explicit"},
		func(ctx context.Context) (AgentResult, error) { return AgentResult{}, nil },
	)
	assertAttr(t, exporter.GetSpans()[0].Attributes, AttrGenAIAgentName, "explicit")
}

// --- helpers ---

func assertAttr(t *testing.T, attrs []attribute.KeyValue, key, want string) {
	t.Helper()
	v, ok := findAttr(attrs, key)
	if !ok {
		t.Errorf("missing attribute %q", key)
		return
	}
	if v.AsString() != want {
		t.Errorf("%s = %q, want %q", key, v.AsString(), want)
	}
}

func assertAttrInt(t *testing.T, attrs []attribute.KeyValue, key string, want int) {
	t.Helper()
	v, ok := findAttr(attrs, key)
	if !ok {
		t.Errorf("missing attribute %q", key)
		return
	}
	if v.AsInt64() != int64(want) {
		t.Errorf("%s = %d, want %d", key, v.AsInt64(), want)
	}
}
