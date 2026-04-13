package amp

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/attribute"
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

	err := tm.AgentSpan(context.Background(), "test-agent", "conv-123",
		[]Message{{Role: "user", Content: "Hello"}},
		func(ctx context.Context) error {
			// Verify we're inside a span.
			span := trace.SpanFromContext(ctx)
			if !span.SpanContext().IsValid() {
				t.Error("expected valid span context")
			}
			return nil
		},
	)

	if err != nil {
		t.Fatalf("AgentSpan() error = %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	s := spans[0]
	attrs := s.Attributes

	// Verify BRD §8 agent span attributes.
	assertAttr(t, attrs, AttrTraceloopSpanKind, "agent")
	assertAttr(t, attrs, AttrGenAIAgentName, "test-agent")
	assertAttr(t, attrs, AttrGenAIConversationID, "conv-123")

	// Verify content capture (TraceContent=true).
	v, ok := findAttr(attrs, AttrGenAIInputMessages)
	if !ok {
		t.Error("expected gen_ai.input.messages attribute")
	}
	if v.AsString() == "" {
		t.Error("gen_ai.input.messages should not be empty")
	}
}

func TestAgentSpan_NoContentCapture(t *testing.T) {
	cfg := Config{
		AgentName:    "test-agent",
		TraceContent: false, // Disabled
	}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	tm.AgentSpan(context.Background(), "test-agent", "conv-123",
		[]Message{{Role: "user", Content: "Hello"}},
		func(ctx context.Context) error { return nil },
	)

	spans := exporter.GetSpans()
	attrs := spans[0].Attributes

	// Should NOT have content attributes.
	if _, ok := findAttr(attrs, AttrGenAIInputMessages); ok {
		t.Error("gen_ai.input.messages should be absent when TraceContent=false")
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
		Temperature: 0.7,
		Messages:    []Message{{Role: "user", Content: "Hello"}},
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

	attrs := spans[0].Attributes

	// Verify BRD §8 LLM span attributes.
	assertAttr(t, attrs, AttrTraceloopSpanKind, "llm")
	assertAttr(t, attrs, AttrGenAISystem, "openai")
	assertAttr(t, attrs, AttrGenAIRequestModel, "gpt-4o")
	assertAttr(t, attrs, AttrGenAIResponseModel, "gpt-4o-2024-05-13")

	// Token usage — both naming conventions.
	assertAttrInt(t, attrs, AttrGenAIUsageInputTokens, 42)
	assertAttrInt(t, attrs, AttrGenAIUsageOutputTokens, 87)
	assertAttrInt(t, attrs, AttrGenAIUsagePromptTokens, 42)
	assertAttrInt(t, attrs, AttrGenAIUsageCompletionTokens, 87)

	// Temperature.
	v, ok := findAttr(attrs, AttrGenAIRequestTemperature)
	if !ok {
		t.Error("missing gen_ai.request.temperature")
	}
	if v.AsFloat64() != 0.7 {
		t.Errorf("temperature = %f, want 0.7", v.AsFloat64())
	}

	// Content capture.
	if _, ok := findAttr(attrs, AttrGenAIInputMessages); !ok {
		t.Error("expected gen_ai.input.messages")
	}
	if _, ok := findAttr(attrs, AttrGenAIOutputMessages); !ok {
		t.Error("expected gen_ai.output.messages")
	}
}

func TestLLMSpan_ResponseModelFallback(t *testing.T) {
	cfg := Config{AgentName: "test", TraceContent: false}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	tm.LLMSpan(context.Background(), LLMRequest{
		Model:    "gpt-4o",
		Provider: "openai",
	}, func(ctx context.Context) (LLMResult, error) {
		return LLMResult{
			Content: "response",
			Model:   "", // Empty — should fall back to request model
		}, nil
	})

	spans := exporter.GetSpans()
	attrs := spans[0].Attributes
	assertAttr(t, attrs, AttrGenAIResponseModel, "gpt-4o")
}

func TestToolSpan_Attributes(t *testing.T) {
	cfg := Config{AgentName: "test", TraceContent: true}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	input := map[string]string{"query": "weather in Nashville"}
	result, err := tm.ToolSpan(context.Background(), "weather_lookup", input,
		func(ctx context.Context) (any, error) {
			return map[string]string{"temp": "72F"}, nil
		},
	)

	if err != nil {
		t.Fatalf("ToolSpan() error = %v", err)
	}
	if result == nil {
		t.Error("expected non-nil result")
	}

	spans := exporter.GetSpans()
	attrs := spans[0].Attributes

	assertAttr(t, attrs, AttrTraceloopSpanKind, "tool")
	assertAttr(t, attrs, AttrGenAIToolName, "weather_lookup")

	if _, ok := findAttr(attrs, AttrGenAIInputMessages); !ok {
		t.Error("expected gen_ai.input.messages for tool input")
	}
	if _, ok := findAttr(attrs, AttrGenAIOutputMessages); !ok {
		t.Error("expected gen_ai.output.messages for tool output")
	}
}

func TestNestedSpans_AgentContainsLLM(t *testing.T) {
	cfg := Config{AgentName: "test-agent", TraceContent: false}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	tm.AgentSpan(context.Background(), "test-agent", "conv-1", nil,
		func(agentCtx context.Context) error {
			_, err := tm.LLMSpan(agentCtx, LLMRequest{
				Model:    "gpt-4o",
				Provider: "openai",
			}, func(llmCtx context.Context) (LLMResult, error) {
				return LLMResult{Content: "response", PromptTokens: 10, CompletionTokens: 20}, nil
			})
			return err
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
	llmKind, _ := findAttr(llmSpan.Attributes, AttrTraceloopSpanKind)
	if llmKind.AsString() != "llm" {
		t.Errorf("LLM span kind = %q, want %q", llmKind.AsString(), "llm")
	}

	agentKind, _ := findAttr(agentSpan.Attributes, AttrTraceloopSpanKind)
	if agentKind.AsString() != "agent" {
		t.Errorf("Agent span kind = %q, want %q", agentKind.AsString(), "agent")
	}
}

func TestNestedSpans_AgentContainsLLMAndTool(t *testing.T) {
	cfg := Config{AgentName: "test-agent", TraceContent: false}
	tm, exporter := newTestTracerManager(t, cfg)
	defer tm.Shutdown(context.Background())

	tm.AgentSpan(context.Background(), "test-agent", "conv-2", nil,
		func(agentCtx context.Context) error {
			// Tool call first.
			_, err := tm.ToolSpan(agentCtx, "search_docs", nil,
				func(toolCtx context.Context) (any, error) {
					return "doc content", nil
				},
			)
			if err != nil {
				return err
			}

			// Then LLM call.
			_, err = tm.LLMSpan(agentCtx, LLMRequest{
				Model:    "gpt-4o",
				Provider: "openai",
			}, func(llmCtx context.Context) (LLMResult, error) {
				return LLMResult{Content: "answer"}, nil
			})
			return err
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
	err := tm.AgentSpan(context.Background(), "test", "conv", nil,
		func(ctx context.Context) error {
			return testErr
		},
	)

	if err != testErr {
		t.Errorf("AgentSpan() error = %v, want %v", err, testErr)
	}

	spans := exporter.GetSpans()
	if len(spans[0].Events) == 0 {
		t.Error("expected error event recorded on span")
	}
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
