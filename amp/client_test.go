package amp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// tokenServer is a stand-in for the AMP API token endpoint (plus /health).
type tokenServer struct {
	t      *testing.T
	mu     sync.Mutex
	paths  []string // request URIs of token requests, in order
	auths  []string
	status int // token endpoint status; 0 means 200
}

func (s *tokenServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		return
	}

	s.mu.Lock()
	s.paths = append(s.paths, r.URL.RequestURI())
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	status := s.status
	s.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		w.Write([]byte(`{"error": "denied"}`))
		return
	}

	expiry := time.Now().Add(time.Hour)
	json.NewEncoder(w).Encode(tokenResponse{
		Token: signTestJWT(s.t, &agentTokenClaims{
			RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(expiry)},
			ComponentUID:     "comp-1",
		}),
		ExpiresAt: expiry.Unix(),
		IssuedAt:  time.Now().Unix(),
		TokenType: "Bearer",
	})
}

func shutdownClient(t *testing.T, c *AMPClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestInit_MissingConfig(t *testing.T) {
	_, err := Init(Config{})
	if err == nil {
		t.Fatal("Init() should fail without AMP_OTEL_ENDPOINT / AMP_AGENT_API_KEY")
	}
	for _, name := range []string{"AMP_OTEL_ENDPOINT", "AMP_AGENT_API_KEY"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("Init() error = %q, should name %s", err, name)
		}
	}
}

func TestInit_WithControlPlane(t *testing.T) {
	otlp := &otlpRecorder{}
	otlpServer := httptest.NewServer(otlp)
	defer otlpServer.Close()

	api := &tokenServer{t: t}
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()

	cfg := Config{
		OTELEndpoint:       otlpServer.URL,
		AgentAPIKey:        "test-api-key",
		BaseURL:            apiServer.URL,
		OrgName:            "org",
		ProjectName:        "proj",
		AgentName:          "agent",
		Environment:        "staging",
		TokenRefreshBuffer: 5 * time.Minute,
		ConversationTTL:    time.Minute,
	}
	client, err := Init(cfg)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer shutdownClient(t, client)

	// The agent JWT was acquired for the configured agent and environment.
	if client.Token == nil || client.Token.Token() == "" {
		t.Fatal("expected an agent JWT when the control plane is configured")
	}
	if got := client.Token.Claims().ComponentUID; got != "comp-1" {
		t.Errorf("ComponentUID = %q, want %q", got, "comp-1")
	}
	wantPath := "/api/v1/orgs/org/projects/proj/agents/agent/token?environment=staging"
	if len(api.paths) != 1 || api.paths[0] != wantPath {
		t.Errorf("token requests = %v, want [%s]", api.paths, wantPath)
	}
	if api.auths[0] != "Bearer test-api-key" {
		t.Errorf("Authorization = %q, want %q", api.auths[0], "Bearer test-api-key")
	}

	if client.Config().AgentName != "agent" {
		t.Errorf("Config().AgentName = %q", client.Config().AgentName)
	}
	if client.Logger() == nil {
		t.Error("Logger() returned nil")
	}

	// Health covers the OTLP endpoint, the AMP API and the JWT.
	health := client.Health(context.Background())
	names := map[string]bool{}
	for _, h := range health {
		names[h.Name] = true
		if !h.Healthy {
			t.Errorf("%s unhealthy: %s", h.Name, h.Error)
		}
	}
	for _, want := range []string{"AMP OTLP endpoint", "AMP API", "Agent JWT"} {
		if !names[want] {
			t.Errorf("Health() missing %q check, got %+v", want, health)
		}
	}
}

func TestInit_FailsFastOnTokenError(t *testing.T) {
	otlpServer := httptest.NewServer(&otlpRecorder{})
	defer otlpServer.Close()

	api := &tokenServer{t: t, status: http.StatusUnauthorized}
	apiServer := httptest.NewServer(api)
	defer apiServer.Close()

	client, err := Init(Config{
		OTELEndpoint: otlpServer.URL,
		AgentAPIKey:  "bad-key",
		BaseURL:      apiServer.URL,
		OrgName:      "org",
		ProjectName:  "proj",
		AgentName:    "agent",
	})
	if err == nil {
		shutdownClient(t, client)
		t.Fatal("Init() should fail when the configured control plane rejects the token request")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("Init() error = %q, should carry the HTTP status", err)
	}
}

func TestInit_PartialControlPlaneIsTracingOnly(t *testing.T) {
	otlpServer := httptest.NewServer(&otlpRecorder{})
	defer otlpServer.Close()

	// AMP_AGENT_NAME alone names the agent; it must not trigger a token request.
	client, err := Init(Config{
		OTELEndpoint:    otlpServer.URL,
		AgentAPIKey:     "key",
		AgentName:       "agent",
		ConversationTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer shutdownClient(t, client)

	if client.Token != nil {
		t.Error("Token manager should be nil without AMP_BASE_URL / org / project")
	}
}

func TestHealth_Unhealthy(t *testing.T) {
	otlp := &otlpRecorder{}
	otlpServer := httptest.NewServer(otlp)
	defer otlpServer.Close()

	// An AMP API address nothing listens on.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	client, err := Init(Config{
		OTELEndpoint:    otlpServer.URL,
		AgentAPIKey:     "key",
		BaseURL:         deadURL,
		ConversationTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	otlp.mu.Lock()
	otlp.status = http.StatusServiceUnavailable
	otlp.mu.Unlock()

	health := client.Health(context.Background())
	if len(health) != 2 {
		t.Fatalf("Health() = %+v, want OTLP endpoint and AMP API checks", health)
	}
	if health[0].Healthy {
		t.Error("OTLP endpoint answering 503 should be unhealthy")
	}
	if health[1].Healthy || health[1].Error == "" {
		t.Errorf("unreachable AMP API should be unhealthy with an error, got %+v", health[1])
	}

	otlp.mu.Lock()
	otlp.status = 0
	otlp.mu.Unlock()
	shutdownClient(t, client)
}

// TestClient_SpansOnTheWire drives every span helper through the client and
// checks the OTLP payload AMP actually receives.
func TestClient_SpansOnTheWire(t *testing.T) {
	otlp := &otlpRecorder{}
	otlpServer := httptest.NewServer(otlp)
	defer otlpServer.Close()

	client, err := Init(Config{
		OTELEndpoint:    otlpServer.URL,
		AgentAPIKey:     "key",
		AgentName:       "wire-agent",
		AgentVersion:    "2.3.4",
		TraceContent:    true,
		ConversationTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	boom := errors.New("tool exploded")
	ctx := WithEvaluationBaggage(context.Background(), "task-1", "trial-1")

	_, err = client.AgentSpan(ctx, AgentRequest{ConversationID: "conv-1"},
		func(ctx context.Context) (AgentResult, error) {
			_, err := client.ChainSpan(ctx, "rag-pipeline", "question",
				func(ctx context.Context) (any, error) {
					client.EmbeddingSpan(ctx, EmbeddingRequest{Model: "embed-1", Provider: "openai", Texts: []string{"question"}},
						func(context.Context) (EmbeddingResult, error) { return EmbeddingResult{InputTokens: 3}, nil })
					client.RetrieverSpan(ctx, RetrieverRequest{VectorDB: "chroma", Collection: "kb", TopK: 3},
						func(context.Context) error { return nil })
					client.RerankSpan(ctx, RerankRequest{Model: "rerank-english-v3.0", Query: "question", CandidateCount: 3},
						func(context.Context) error { return nil })
					client.LLMSpan(ctx, LLMRequest{Model: "gpt-4o", Provider: "openai", Temperature: Float64(0.2)},
						func(context.Context) (LLMResult, error) {
							return LLMResult{Content: "answer", PromptTokens: 5, CompletionTokens: 7}, nil
						})
					client.ToolSpan(ctx, ToolRequest{Name: "word_count", CallID: "call_1"},
						func(context.Context) (any, error) { return nil, boom })
					return "answer", nil
				})
			return AgentResult{
				Messages:     []Message{{Role: "assistant", Content: "answer"}},
				InputTokens:  5,
				OutputTokens: 7,
			}, err
		})
	if err != nil {
		t.Fatalf("AgentSpan() error = %v", err)
	}

	shutdownClient(t, client)

	spans := otlp.spans()
	wantKinds := map[string][2]string{ // span name → identifying attribute
		"invoke_agent":  {AttrGenAIOperationName, "invoke_agent"},
		"rag-pipeline":  {AttrTraceloopSpanKind, "workflow"},
		"embeddings":    {AttrGenAIOperationName, "embeddings"},
		"vector_search": {AttrDBSystemName, "chroma"},
		"rerank":        {AttrTraceloopSpanKind, "rerank"},
		"chat":          {AttrGenAIOperationName, "chat"},
		"execute_tool":  {AttrGenAIOperationName, "execute_tool"},
	}
	if len(spans) != len(wantKinds) {
		t.Fatalf("exported %d spans, want %d: %v", len(spans), len(wantKinds), spans)
	}

	agent := spans["invoke_agent"]
	chain := spans["rag-pipeline"]
	for name, want := range wantKinds {
		sp, ok := spans[name]
		if !ok {
			t.Errorf("span %q was not exported", name)
			continue
		}
		if sp.Attrs[want[0]] != want[1] {
			t.Errorf("%s: %s = %q, want %q", name, want[0], sp.Attrs[want[0]], want[1])
		}
		if sp.TraceID != agent.TraceID {
			t.Errorf("%s is not in the agent's trace", name)
		}
		// Resource and instrumentation scope identify the agent and SDK.
		if sp.Resource["service.name"] != "wire-agent" {
			t.Errorf("%s: service.name = %q, want %q", name, sp.Resource["service.name"], "wire-agent")
		}
		if sp.Resource["agent-manager/agent-version"] != "2.3.4" {
			t.Errorf("%s: agent-manager/agent-version = %q", name, sp.Resource["agent-manager/agent-version"])
		}
		if sp.ScopeName != "amp-go" || sp.ScopeVersion != Version {
			t.Errorf("%s: scope = %s@%s, want amp-go@%s", name, sp.ScopeName, sp.ScopeVersion, Version)
		}

		// Trace shape: agent → chain → everything else.
		switch name {
		case "invoke_agent":
			if sp.ParentSpanID != "" {
				t.Errorf("agent span should be the root, parent = %q", sp.ParentSpanID)
			}
		case "rag-pipeline":
			if sp.ParentSpanID != agent.SpanID {
				t.Error("chain span should be a child of the agent span")
			}
		default:
			if sp.ParentSpanID != chain.SpanID {
				t.Errorf("%s should be a child of the chain span", name)
			}
		}
	}

	if agent.Attrs[AttrGenAIAgentName] != "wire-agent" {
		t.Errorf("gen_ai.agent.name = %q, want %q", agent.Attrs[AttrGenAIAgentName], "wire-agent")
	}
	if agent.Attrs[AttrTaskID] != "task-1" || agent.Attrs[AttrTrialID] != "trial-1" {
		t.Errorf("evaluation correlation = %q/%q", agent.Attrs[AttrTaskID], agent.Attrs[AttrTrialID])
	}
	if agent.Attrs[AttrGenAIUsageOutputTokens] != "7" {
		t.Errorf("agent output tokens = %q, want 7", agent.Attrs[AttrGenAIUsageOutputTokens])
	}
	if spans["chat"].Attrs[AttrGenAIRequestTemperature] != "0.2" {
		t.Errorf("temperature = %q, want 0.2", spans["chat"].Attrs[AttrGenAIRequestTemperature])
	}

	// Only the failed tool carries the error badge.
	for name, sp := range spans {
		if wantErr := name == "execute_tool"; sp.StatusError != wantErr {
			t.Errorf("%s: error status = %v, want %v", name, sp.StatusError, wantErr)
		}
	}
	if spans["execute_tool"].Attrs[AttrErrorType] == "" {
		t.Error("failed tool span is missing error.type")
	}
}

func TestClient_Middleware(t *testing.T) {
	otlpServer := httptest.NewServer(&otlpRecorder{})
	defer otlpServer.Close()

	client, err := Init(Config{
		OTELEndpoint:    otlpServer.URL,
		AgentAPIKey:     "key",
		ConversationTTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer shutdownClient(t, client)

	var convID string
	var scope []string
	handler := client.ConversationMiddleware()(
		client.LeastPrivilegeMiddleware([]string{"read:docs", "read:orders"})(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				convID = ConversationIDFromContext(r.Context())
				scope = EffectiveScopeFromContext(r.Context())
			}),
		),
	)

	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header.Set("X-Conversation-ID", "conv-7")
	req.Header.Set("X-User-Token", signTestMapJWT(t, jwt.MapClaims{"scope": "read:docs write:all"}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if convID != "conv-7" || rr.Header().Get("X-Conversation-ID") != "conv-7" {
		t.Errorf("conversation ID = %q (header %q), want conv-7", convID, rr.Header().Get("X-Conversation-ID"))
	}
	if !slicesEqual(scope, []string{"read:docs"}) {
		t.Errorf("effective scope = %v, want [read:docs]", scope)
	}
	if client.Conversation.Get("conv-7") == nil {
		t.Error("conversation should be tracked by the client's manager")
	}
}
