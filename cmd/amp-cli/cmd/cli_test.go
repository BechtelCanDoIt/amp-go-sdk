package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/wso2/amp-go/amp"
)

// captureStdout runs fn and returns everything it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	defer func() { os.Stdout = orig }()
	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}

// useSDKClient installs an initialized SDK client as the CLI's shared client,
// backed by a stub OTLP endpoint, and restores the CLI globals afterwards.
func useSDKClient(t *testing.T, cfg amp.Config) *amp.AMPClient {
	t.Helper()

	otlp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cfg.OTELEndpoint = otlp.URL
	cfg.AgentAPIKey = "test-api-key"
	cfg.ConversationTTL = time.Minute

	c, err := amp.Init(cfg)
	if err != nil {
		otlp.Close()
		t.Fatalf("amp.Init() error = %v", err)
	}
	client = c

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.Shutdown(ctx)
		otlp.Close()
		client = nil
		agentURL, userToken, debug = "http://localhost:8080", "", false
		traceLimit, traceFormat, traceSince, traceToken = 10, "table", 24*time.Hour, ""
		sendMessage, sendConversationID, sendOutputJSON = "", "", false
		tokenOutputJSON = false
	})
	return c
}

// newTokenAPI is a stub AMP API that issues an agent JWT.
func newTokenAPI(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	expiry := time.Now().Add(time.Hour)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"exp":             expiry.Unix(),
		"component_uid":   "comp-1",
		"environment_uid": "env-1",
		"project_uid":     "proj-1",
		"org_id":          "org-1",
		"namespace":       "acme-dev",
	})
	raw, err := token.SignedString([]byte("test-secret"))
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"token":      raw,
			"expires_at": expiry.Unix(),
			"issued_at":  time.Now().Unix(),
			"token_type": "Bearer",
		})
	}))
	t.Cleanup(server.Close)
	return server, raw
}

const tracesBody = `{"traces":[{"traceId":"abc123","rootSpanName":"invoke_agent","rootSpanKind":"agent",
"spanCount":3,"durationInNanos":42000000,"tokenUsage":{"inputTokens":11,"outputTokens":7},
"input":"hello\nworld"}],"totalCount":1}`

// --- trace ---

func TestTrace_QueriesObserver(t *testing.T) {
	var got *http.Request
	observer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Write([]byte(tracesBody))
	}))
	defer observer.Close()

	useSDKClient(t, amp.Config{
		TraceObserverURL: observer.URL,
		OrgName:          "org",
		ProjectName:      "proj",
		AgentName:        "agent",
		Environment:      "staging",
	})
	traceLimit, traceSince, traceToken = 5, time.Hour, "observer-token"

	var err error
	out := captureStdout(t, func() { err = runTrace(traceCmd, nil) })
	if err != nil {
		t.Fatalf("runTrace() error = %v", err)
	}

	// The request matches the agent-manager-observer trace query API.
	if got.URL.Path != "/api/v1/traces" {
		t.Errorf("path = %q, want /api/v1/traces", got.URL.Path)
	}
	q := got.URL.Query()
	want := url.Values{
		"organization": {"org"}, "project": {"proj"}, "agent": {"agent"},
		"environment": {"staging"}, "limit": {"5"}, "sortOrder": {"desc"},
	}
	for k, v := range want {
		if q.Get(k) != v[0] {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), v[0])
		}
	}
	start, err1 := time.Parse(time.RFC3339, q.Get("startTime"))
	end, err2 := time.Parse(time.RFC3339, q.Get("endTime"))
	if err1 != nil || err2 != nil {
		t.Fatalf("startTime/endTime are not RFC3339: %q / %q", q.Get("startTime"), q.Get("endTime"))
	}
	if window := end.Sub(start); window != time.Hour {
		t.Errorf("time window = %s, want --since of 1h", window)
	}
	if got.Header.Get("Authorization") != "Bearer observer-token" {
		t.Errorf("Authorization = %q, want the --token value", got.Header.Get("Authorization"))
	}

	for _, want := range []string{"abc123", "agent", "11i 7o", "42ms", "hello world", "showing 1 of 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestTrace_FallsBackToAgentJWT(t *testing.T) {
	api, agentJWT := newTokenAPI(t)

	var auth string
	observer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write([]byte(`{"traces":[],"totalCount":0}`))
	}))
	defer observer.Close()

	useSDKClient(t, amp.Config{
		TraceObserverURL: observer.URL,
		BaseURL:          api.URL,
		OrgName:          "org",
		ProjectName:      "proj",
		AgentName:        "agent",
		Environment:      "staging",
	})

	var err error
	out := captureStdout(t, func() { err = runTrace(traceCmd, nil) })
	if err != nil {
		t.Fatalf("runTrace() error = %v", err)
	}
	if auth != "Bearer "+agentJWT {
		t.Errorf("Authorization = %q, want the agent JWT", auth)
	}
	if !strings.Contains(out, "No traces found for agent 'agent'") {
		t.Errorf("expected the empty-result message, got:\n%s", out)
	}
}

func TestTrace_JSONFormat(t *testing.T) {
	observer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tracesBody))
	}))
	defer observer.Close()

	useSDKClient(t, amp.Config{
		TraceObserverURL: observer.URL,
		OrgName:          "org", ProjectName: "proj", AgentName: "agent", Environment: "staging",
	})
	traceFormat = "json"

	var err error
	out := captureStdout(t, func() { err = runTrace(traceCmd, nil) })
	if err != nil {
		t.Fatalf("runTrace() error = %v", err)
	}

	var decoded tracesResponse
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if decoded.TotalCount != 1 || decoded.Traces[0].TraceID != "abc123" {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestTrace_Errors(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer failing.Close()

	full := amp.Config{
		TraceObserverURL: failing.URL,
		OrgName:          "org", ProjectName: "proj", AgentName: "agent", Environment: "staging",
	}
	noObserver, noEnv, noOrg := full, full, full
	noObserver.TraceObserverURL = ""
	noEnv.Environment = ""
	noOrg.OrgName = ""

	tests := []struct {
		name    string
		cfg     amp.Config
		wantErr string
	}{
		{"observer URL not set", noObserver, "AMP_TRACE_OBSERVER_URL"},
		{"environment not set", noEnv, "AMP_ENVIRONMENT"},
		{"org not set", noOrg, "AMP_ORG_NAME"},
		{"observer rejects the request", full, "HTTP 401"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useSDKClient(t, tt.cfg)
			var err error
			captureStdout(t, func() { err = runTrace(traceCmd, nil) })
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("runTrace() error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 40); got != "short" {
		t.Errorf("truncate() = %q", got)
	}
	if got := truncate("a\nb", 40); got != "a b" {
		t.Errorf("truncate() should flatten newlines, got %q", got)
	}
	if got := truncate(strings.Repeat("x", 50), 10); got != "xxxxxxx..." {
		t.Errorf("truncate() = %q, want 7 chars plus ellipsis", got)
	}
}

// --- token ---

func TestToken_RequiresControlPlane(t *testing.T) {
	useSDKClient(t, amp.Config{})

	err := runToken(tokenCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "AMP_BASE_URL") {
		t.Errorf("runToken() error = %v, want it to explain the missing control plane", err)
	}
}

func TestToken_PrintsClaims(t *testing.T) {
	api, agentJWT := newTokenAPI(t)
	useSDKClient(t, amp.Config{
		BaseURL: api.URL, OrgName: "org", ProjectName: "proj", AgentName: "agent",
	})

	tokenOutputJSON = true
	var err error
	out := captureStdout(t, func() { err = runToken(tokenCmd, nil) })
	if err != nil {
		t.Fatalf("runToken() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	want := map[string]any{
		"token": agentJWT, "component_uid": "comp-1", "environment_uid": "env-1",
		"project_uid": "proj-1", "org_id": "org-1", "namespace": "acme-dev", "expired": false,
	}
	for k, v := range want {
		if decoded[k] != v {
			t.Errorf("%s = %v, want %v", k, decoded[k], v)
		}
	}

	// Human-readable form names the agent and shows the claims.
	tokenOutputJSON = false
	out = captureStdout(t, func() { err = runToken(tokenCmd, nil) })
	if err != nil {
		t.Fatalf("runToken() error = %v", err)
	}
	for _, want := range []string{"Agent JWT for 'agent'", "comp-1", "org-1", "acme-dev"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, agentJWT) {
		t.Error("the full token must only be printed with --debug")
	}
}

// --- health ---

func TestHealth_Report(t *testing.T) {
	c := useSDKClient(t, amp.Config{})

	out := captureStdout(t, func() { runHealthCheck(c) })
	for _, want := range []string{"AMP Health Check", "✓ AMP OTLP endpoint", "/v1/traces", "All services healthy"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestHealth_ReportsUnhealthy(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	c := useSDKClient(t, amp.Config{BaseURL: deadURL})

	out := captureStdout(t, func() { runHealthCheck(c) })
	for _, want := range []string{"✗ AMP API", "Error:", "WARNING: Some services are unhealthy"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// --- send / chat ---

// agentStub is a stand-in for an agent's /chat endpoint.
type agentStub struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []chatRequest
	status   int
}

func (a *agentStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body chatRequest
	json.NewDecoder(r.Body).Decode(&body)

	a.mu.Lock()
	a.requests = append(a.requests, r)
	a.bodies = append(a.bodies, body)
	status := a.status
	a.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		w.Write([]byte(`{"error":"no effective scope"}`))
		return
	}
	w.Header().Set("X-Conversation-ID", "server-conv")
	w.Write([]byte(`{"reply":"hi from agent","conversation_id":"server-conv","tokens":{"prompt":4,"completion":2},"latency_ms":9}`))
}

func newAgentStub(t *testing.T) *agentStub {
	t.Helper()
	stub := &agentStub{}
	server := httptest.NewServer(stub)
	t.Cleanup(func() {
		server.Close()
		agentURL, userToken, debug = "http://localhost:8080", "", false
		sendMessage, sendConversationID, sendOutputJSON = "", "", false
	})
	agentURL = server.URL
	return stub
}

func TestSend(t *testing.T) {
	stub := newAgentStub(t)
	sendMessage, sendConversationID, userToken = "Hello", "conv-9", "employee-jwt"

	var err error
	out := captureStdout(t, func() { err = runSend(sendCmd, nil) })
	if err != nil {
		t.Fatalf("runSend() error = %v", err)
	}
	if strings.TrimSpace(out) != "hi from agent" {
		t.Errorf("output = %q, want just the reply", out)
	}

	req := stub.requests[0]
	if req.Method != http.MethodPost || req.URL.Path != "/chat" {
		t.Errorf("request = %s %s, want POST /chat", req.Method, req.URL.Path)
	}
	if stub.bodies[0].Message != "Hello" {
		t.Errorf("message = %q, want Hello", stub.bodies[0].Message)
	}
	if req.Header.Get("X-Conversation-ID") != "conv-9" {
		t.Errorf("X-Conversation-ID = %q, want conv-9", req.Header.Get("X-Conversation-ID"))
	}
	if req.Header.Get("X-User-Token") != "employee-jwt" {
		t.Errorf("X-User-Token = %q, want employee-jwt", req.Header.Get("X-User-Token"))
	}
}

func TestSend_NewConversationOmitsHeaders(t *testing.T) {
	stub := newAgentStub(t)
	sendMessage = "Hello"

	captureStdout(t, func() { runSend(sendCmd, nil) })

	req := stub.requests[0]
	if _, ok := req.Header["X-Conversation-Id"]; ok {
		t.Error("X-Conversation-ID should be absent so the agent starts a new conversation")
	}
	if _, ok := req.Header["X-User-Token"]; ok {
		t.Error("X-User-Token should be absent without --user-token")
	}
}

func TestSend_JSONOutput(t *testing.T) {
	newAgentStub(t)
	sendMessage, sendOutputJSON = "Hello", true

	var err error
	out := captureStdout(t, func() { err = runSend(sendCmd, nil) })
	if err != nil {
		t.Fatalf("runSend() error = %v", err)
	}

	var decoded struct {
		Reply          string         `json:"reply"`
		ConversationID string         `json:"conversation_id"`
		Tokens         map[string]int `json:"tokens"`
		ServerLatency  int            `json:"server_latency_ms"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if decoded.Reply != "hi from agent" || decoded.ConversationID != "server-conv" {
		t.Errorf("decoded = %+v", decoded)
	}
	if decoded.Tokens["prompt"] != 4 || decoded.Tokens["completion"] != 2 || decoded.ServerLatency != 9 {
		t.Errorf("token/latency fields = %+v", decoded)
	}
}

func TestSend_AgentError(t *testing.T) {
	stub := newAgentStub(t)
	stub.status = http.StatusForbidden
	sendMessage = "Hello"

	var err error
	captureStdout(t, func() { err = runSend(sendCmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "no effective scope") {
		t.Errorf("runSend() error = %v, want the agent's status and body", err)
	}
}

func TestChat_MultiTurn(t *testing.T) {
	stub := newAgentStub(t)
	userToken = "employee-jwt"

	// Feed the REPL two messages (and a blank line) then exit.
	origStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()
	go func() {
		io.WriteString(w, "first\n\nsecond\nexit\n")
		w.Close()
	}()

	out := captureStdout(t, func() { err = runChat(chatCmd, nil) })
	if err != nil {
		t.Fatalf("runChat() error = %v", err)
	}

	if len(stub.bodies) != 2 || stub.bodies[0].Message != "first" || stub.bodies[1].Message != "second" {
		t.Fatalf("agent received %+v, want the two messages", stub.bodies)
	}
	// Turn 1 carries the generated ID; turn 2 echoes the one the agent returned.
	if first := stub.requests[0].Header.Get("X-Conversation-ID"); first == "" || first == "server-conv" {
		t.Errorf("turn 1 X-Conversation-ID = %q, want a generated ID", first)
	}
	if second := stub.requests[1].Header.Get("X-Conversation-ID"); second != "server-conv" {
		t.Errorf("turn 2 X-Conversation-ID = %q, want the server's ID", second)
	}
	if stub.requests[1].Header.Get("X-User-Token") != "employee-jwt" {
		t.Error("X-User-Token should be sent on every turn")
	}
	for _, want := range []string{"Agent: hi from agent", "tokens: 4p 2c", "Goodbye!"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestChat_AgentErrorKeepsSessionAlive(t *testing.T) {
	stub := newAgentStub(t)
	stub.status = http.StatusForbidden

	origStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()
	go func() {
		io.WriteString(w, "hello\nquit\n")
		w.Close()
	}()

	var err error
	out := captureStdout(t, func() { err = runChat(chatCmd, nil) })
	if err != nil {
		t.Fatalf("runChat() error = %v", err)
	}
	if !strings.Contains(out, "Error 403") || !strings.Contains(out, "Goodbye!") {
		t.Errorf("expected the error to be shown and the REPL to continue:\n%s", out)
	}
}

// --- sleep ---

func TestSleep_WaitsThenExits(t *testing.T) {
	start := time.Now()
	if err := sleepCmd.RunE(sleepCmd, []string{"150ms"}); err != nil {
		t.Fatalf("sleep error = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("sleep 150ms returned after %s", elapsed)
	}
}

func TestSleep_StopsWhenCancelled(t *testing.T) {
	// Pod deletion sends SIGTERM, which cancels the context: the process must
	// exit promptly rather than sit out the remaining hour.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sleepFor(ctx, time.Hour)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sleepFor did not return after its context was cancelled")
	}
}

func TestSleep_RejectsBadDuration(t *testing.T) {
	// "3600" is what the manifest used to pass to the missing sleep binary.
	for _, arg := range []string{"3600", "soon", "-5m"} {
		if err := sleepCmd.RunE(sleepCmd, []string{arg}); err == nil {
			t.Errorf("sleep %q should be rejected", arg)
		}
	}
	if err := sleepCmd.Args(sleepCmd, nil); err == nil {
		t.Error("sleep with no duration should be rejected")
	}
}
