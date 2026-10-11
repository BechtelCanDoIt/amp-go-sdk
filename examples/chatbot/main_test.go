package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wso2/amp-go/amp"
)

// llmStub is a stand-in for an OpenAI-compatible /chat/completions backend.
type llmStub struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []openAIChatRequest
	status   int
}

func (l *llmStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body openAIChatRequest
	json.NewDecoder(r.Body).Decode(&body)

	l.mu.Lock()
	l.requests = append(l.requests, r)
	l.bodies = append(l.bodies, body)
	status := l.status
	l.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		w.Write([]byte(`{"error":"model overloaded"}`))
		return
	}
	w.Write([]byte(`{"id":"x","model":"stub-model-1",
		"choices":[{"message":{"role":"assistant","content":"stub answer"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
}

// newTestChatbot wires the chatbot's handler chain exactly as main() does,
// against stub OTLP and LLM backends.
func newTestChatbot(t *testing.T) (http.Handler, *llmStub) {
	t.Helper()

	otlp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	llm := &llmStub{}
	llmServer := httptest.NewServer(llm)

	client, err := amp.Init(amp.Config{
		OTELEndpoint:    otlp.URL,
		AgentAPIKey:     "test-api-key",
		AgentName:       "chatbot",
		TraceContent:    true,
		ConversationTTL: time.Minute,
		LLMBaseURL:      llmServer.URL + "/v1/",
		LLMAPIKey:       "llm-key",
		LLMModel:        "stub-model",
	})
	if err != nil {
		t.Fatalf("amp.Init() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client.Shutdown(ctx)
		llmServer.Close()
		otlp.Close()
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatHandler(w, r, client, client.Logger())
	})
	chain := client.ConversationMiddleware()(
		client.LeastPrivilegeMiddleware([]string{"read:docs", "read:orders"})(handler),
	)
	return chain, llm
}

func postChat(t *testing.T, h http.Handler, body string, headers map[string]string) (*httptest.ResponseRecorder, chatResponseBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	var resp chatResponseBody
	json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr, resp
}

// unsignedJWT builds a token carrying the given scope claim; the middleware
// decodes without verifying, as the gateway has already done that.
func unsignedJWT(scope string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none","typ":"JWT"}`) + "." + enc(`{"scope":"`+scope+`"}`) + "."
}

func TestChat_AMPChatAgentContract(t *testing.T) {
	h, llm := newTestChatbot(t)

	rr, resp := postChat(t, h, `{"session_id":"demo-1","message":"Hello"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}

	// {session_id, message} in → {response, session_id} out.
	if resp.Response != "stub answer" || resp.SessionID != "demo-1" {
		t.Errorf("response = %q, session_id = %q", resp.Response, resp.SessionID)
	}
	// amp-cli reads the mirrored fields.
	if resp.Reply != "stub answer" || resp.ConversationID != "demo-1" {
		t.Errorf("reply = %q, conversation_id = %q", resp.Reply, resp.ConversationID)
	}
	if resp.Tokens.Prompt != 11 || resp.Tokens.Completion != 7 {
		t.Errorf("tokens = %+v, want 11/7", resp.Tokens)
	}
	if rr.Header().Get("X-Conversation-ID") != "demo-1" {
		t.Errorf("X-Conversation-ID = %q, want demo-1", rr.Header().Get("X-Conversation-ID"))
	}

	// The LLM backend got an OpenAI-compatible request.
	req, body := llm.requests[0], llm.bodies[0]
	if req.URL.Path != "/v1/chat/completions" {
		t.Errorf("LLM path = %q, want /v1/chat/completions", req.URL.Path)
	}
	if req.Header.Get("Authorization") != "Bearer llm-key" {
		t.Errorf("LLM Authorization = %q", req.Header.Get("Authorization"))
	}
	if body.Model != "stub-model" || body.Temperature != llmTemperature {
		t.Errorf("LLM model/temperature = %q/%v", body.Model, body.Temperature)
	}
	if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Content != "Hello" {
		t.Errorf("LLM messages = %+v, want system prompt + user message", body.Messages)
	}
	// Trace context is propagated so the LLM call joins the agent's trace.
	if req.Header.Get("traceparent") == "" {
		t.Error("LLM request is missing the traceparent header")
	}
}

func TestChat_MultiTurnHistory(t *testing.T) {
	h, llm := newTestChatbot(t)

	postChat(t, h, `{"session_id":"s1","message":"first"}`, nil)
	postChat(t, h, `{"session_id":"s1","message":"second"}`, nil)
	postChat(t, h, `{"session_id":"other","message":"unrelated"}`, nil)

	// Turn 2 replays turn 1; a different session starts clean.
	var contents []string
	for _, m := range llm.bodies[1].Messages {
		contents = append(contents, m.Role+":"+m.Content)
	}
	want := []string{"user:first", "assistant:stub answer", "user:second"}
	if got := contents[1:]; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("turn 2 messages = %v, want history then the new message %v", got, want)
	}
	if n := len(llm.bodies[2].Messages); n != 2 {
		t.Errorf("a new session sent %d messages, want 2 (system + user)", n)
	}
}

func TestChat_ConversationFromHeader(t *testing.T) {
	h, _ := newTestChatbot(t)

	// No session_id: the header identifies the conversation.
	rr, resp := postChat(t, h, `{"message":"Hello"}`, map[string]string{"X-Conversation-ID": "hdr-1"})
	if resp.SessionID != "hdr-1" || rr.Header().Get("X-Conversation-ID") != "hdr-1" {
		t.Errorf("session_id = %q, header = %q, want hdr-1", resp.SessionID, rr.Header().Get("X-Conversation-ID"))
	}

	// Neither: an ID is generated and returned for the client to echo.
	rr, resp = postChat(t, h, `{"message":"Hello"}`, nil)
	if resp.SessionID == "" || resp.SessionID != rr.Header().Get("X-Conversation-ID") {
		t.Errorf("generated session_id = %q, header = %q", resp.SessionID, rr.Header().Get("X-Conversation-ID"))
	}
}

func TestChat_EvaluationBaggagePropagates(t *testing.T) {
	h, llm := newTestChatbot(t)

	postChat(t, h, `{"session_id":"s1","message":"Hello","task_id":"task-1","trial_id":"trial-1"}`, nil)

	baggage := llm.requests[0].Header.Get("baggage")
	if !strings.Contains(baggage, "task_id=task-1") || !strings.Contains(baggage, "trial_id=trial-1") {
		t.Errorf("baggage header = %q, want task_id and trial_id", baggage)
	}
}

func TestChat_LeastPrivilege(t *testing.T) {
	h, llm := newTestChatbot(t)

	// Overlapping scopes: the effective scope reaches the system prompt.
	rr, _ := postChat(t, h, `{"message":"Hello"}`, map[string]string{"X-User-Token": unsignedJWT("read:docs write:all")})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body)
	}
	system := llm.bodies[0].Messages[0].Content
	if !strings.Contains(system, "read:docs") || strings.Contains(system, "read:orders") {
		t.Errorf("system prompt should carry only the effective scope, got %q", system)
	}

	// No overlap: rejected before the LLM is called.
	rr, _ = postChat(t, h, `{"message":"Hello"}`, map[string]string{"X-User-Token": unsignedJWT("write:all")})
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
	if len(llm.requests) != 1 {
		t.Errorf("LLM was called %d times, want 1", len(llm.requests))
	}
}

func TestChat_BadRequests(t *testing.T) {
	h, llm := newTestChatbot(t)

	tests := []struct {
		name, method, body string
		want               int
	}{
		{"wrong method", http.MethodGet, ``, http.StatusMethodNotAllowed},
		{"invalid JSON", http.MethodPost, `{not json`, http.StatusBadRequest},
		{"missing message", http.MethodPost, `{"session_id":"s1"}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/chat", strings.NewReader(tt.body))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != tt.want {
				t.Errorf("status = %d, want %d", rr.Code, tt.want)
			}
		})
	}
	if len(llm.requests) != 0 {
		t.Errorf("LLM was called %d times for invalid requests", len(llm.requests))
	}
}

func TestChat_LLMFailure(t *testing.T) {
	h, llm := newTestChatbot(t)
	llm.status = http.StatusServiceUnavailable

	rr, _ := postChat(t, h, `{"session_id":"s1","message":"Hello"}`, nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	var body map[string]string
	json.Unmarshal(rr.Body.Bytes(), &body)
	if !strings.Contains(body["error"], "503") {
		t.Errorf("error = %q, want the LLM status", body["error"])
	}

	// A failed turn must not be recorded as conversation history.
	llm.mu.Lock()
	llm.status = 0
	llm.mu.Unlock()
	postChat(t, h, `{"session_id":"s1","message":"again"}`, nil)
	if n := len(llm.bodies[1].Messages); n != 2 {
		t.Errorf("retry sent %d messages, want 2 (system + user)", n)
	}
}

func TestDetectProvider(t *testing.T) {
	tests := map[string]string{
		"http://localhost:11434/v1":       "ollama",
		"http://ollama:8000/v1":           "ollama",
		"https://api.openai.com/v1":       "openai",
		"https://api.anthropic.com/v1":    "anthropic",
		"https://apim-gateway:8243/ai/v1": "openai",
	}
	for url, want := range tests {
		if got := detectProvider(url); got != want {
			t.Errorf("detectProvider(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestParseAgentScopes(t *testing.T) {
	t.Setenv("AGENT_SCOPES", "")
	if got := parseAgentScopes(); len(got) != 1 || got[0] != "read:all" {
		t.Errorf("default scopes = %v, want [read:all]", got)
	}

	t.Setenv("AGENT_SCOPES", "read:docs  read:orders")
	if got := parseAgentScopes(); len(got) != 2 || got[0] != "read:docs" || got[1] != "read:orders" {
		t.Errorf("scopes = %v, want [read:docs read:orders]", got)
	}
}
