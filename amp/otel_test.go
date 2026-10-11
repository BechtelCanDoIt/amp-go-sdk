package amp

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// otelTracerForTest returns a tracer from the global provider, the way an
// agent using InitOTel() directly would.
func otelTracerForTest() trace.Tracer {
	return otel.Tracer("amp-go-test")
}

// otlpRecorder is a stand-in for the AMP OTLP gateway. It keeps every export
// request and decodes the payload, so tests can assert on what actually goes
// over the wire.
type otlpRecorder struct {
	mu       sync.Mutex
	requests []*http.Request
	exports  []*coltracepb.ExportTraceServiceRequest
	status   int // response status; 0 means 200
}

func (r *otlpRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)

	r.mu.Lock()
	r.requests = append(r.requests, req)
	if req.Method == http.MethodPost {
		var export coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &export); err == nil {
			r.exports = append(r.exports, &export)
		}
	}
	status := r.status
	r.mu.Unlock()

	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

func (r *otlpRecorder) last(t *testing.T) *http.Request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		t.Fatal("no OTLP export request reached the endpoint")
	}
	return r.requests[len(r.requests)-1]
}

// wireSpan is one exported span, flattened for assertions.
type wireSpan struct {
	Name         string
	SpanID       string
	ParentSpanID string
	TraceID      string
	Attrs        map[string]string
	Resource     map[string]string
	ScopeName    string
	ScopeVersion string
	StatusError  bool
}

// spans returns every span received so far, keyed by span name.
func (r *otlpRecorder) spans() map[string]wireSpan {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := map[string]wireSpan{}
	for _, export := range r.exports {
		for _, rs := range export.GetResourceSpans() {
			resource := wireAttrs(rs.GetResource().GetAttributes())
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					out[sp.GetName()] = wireSpan{
						Name:         sp.GetName(),
						SpanID:       hex.EncodeToString(sp.GetSpanId()),
						ParentSpanID: hex.EncodeToString(sp.GetParentSpanId()),
						TraceID:      hex.EncodeToString(sp.GetTraceId()),
						Attrs:        wireAttrs(sp.GetAttributes()),
						Resource:     resource,
						ScopeName:    ss.GetScope().GetName(),
						ScopeVersion: ss.GetScope().GetVersion(),
						StatusError:  sp.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR,
					}
				}
			}
		}
	}
	return out
}

func wireAttrs(kvs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_StringValue:
			out[kv.GetKey()] = v.StringValue
		case *commonpb.AnyValue_IntValue:
			out[kv.GetKey()] = strconv.FormatInt(v.IntValue, 10)
		case *commonpb.AnyValue_DoubleValue:
			out[kv.GetKey()] = strconv.FormatFloat(v.DoubleValue, 'f', -1, 64)
		case *commonpb.AnyValue_BoolValue:
			out[kv.GetKey()] = strconv.FormatBool(v.BoolValue)
		}
	}
	return out
}

// assertTransport checks the transport half of the contract: OTLP/HTTP POST to
// <AMP_OTEL_ENDPOINT>/v1/traces with the x-amp-api-key header.
func assertTransport(t *testing.T, req *http.Request, wantPath, wantKey string) {
	t.Helper()
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.URL.Path != wantPath {
		t.Errorf("path = %q, want %q", req.URL.Path, wantPath)
	}
	if got := req.Header.Get("x-amp-api-key"); got != wantKey {
		t.Errorf("x-amp-api-key = %q, want %q", got, wantKey)
	}
}

func TestInit_TracingOnly(t *testing.T) {
	rec := &otlpRecorder{}
	server := httptest.NewServer(rec)
	defer server.Close()

	// Only the two required settings — no control plane, so no agent JWT.
	client, err := Init(Config{
		OTELEndpoint:    server.URL + "/otel",
		AgentAPIKey:     "test-api-key",
		TraceContent:    true,
		ConversationTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if client.Token != nil {
		t.Error("Token manager should be nil when the control plane is not configured")
	}

	client.LLMSpan(context.Background(), LLMRequest{Model: "gpt-4o", Provider: "openai"},
		func(context.Context) (LLMResult, error) { return LLMResult{Content: "hi"}, nil },
	)

	health := client.Health(context.Background())
	if len(health) != 1 || !health[0].Healthy {
		t.Errorf("Health() = %+v, want one healthy OTLP endpoint check", health)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	assertTransport(t, rec.last(t), "/otel/v1/traces", "test-api-key")
}

func TestInitOTel(t *testing.T) {
	rec := &otlpRecorder{}
	server := httptest.NewServer(rec)
	defer server.Close()

	t.Setenv("AMP_OTEL_ENDPOINT", server.URL)
	t.Setenv("AMP_AGENT_API_KEY", "env-api-key")
	t.Cleanup(func() { initOTelShutdown = nil })

	shutdown, err := InitOTel()
	if err != nil {
		t.Fatalf("InitOTel() error = %v", err)
	}

	// Idempotent: a second call must not build a second provider.
	again, err := InitOTel()
	if err != nil {
		t.Fatalf("second InitOTel() error = %v", err)
	}
	if again == nil {
		t.Fatal("second InitOTel() returned nil shutdown")
	}

	tm := &TracerManager{cfg: FromEnv(), tracer: otelTracerForTest()}
	tm.LLMSpan(context.Background(), LLMRequest{Model: "gpt-4o", Provider: "openai"},
		func(context.Context) (LLMResult, error) { return LLMResult{Content: "hi"}, nil },
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown error = %v", err)
	}

	rec.mu.Lock()
	n := len(rec.requests)
	rec.mu.Unlock()
	if n != 1 {
		t.Errorf("got %d export requests, want 1 (provider registered twice?)", n)
	}
	assertTransport(t, rec.last(t), "/v1/traces", "env-api-key")
}

func TestInitOTel_MissingEnv(t *testing.T) {
	t.Setenv("AMP_OTEL_ENDPOINT", "")
	t.Setenv("AMP_AGENT_API_KEY", "")
	t.Cleanup(func() { initOTelShutdown = nil })

	if _, err := InitOTel(); err == nil {
		t.Fatal("InitOTel() should fail without AMP_OTEL_ENDPOINT / AMP_AGENT_API_KEY")
	}
}
