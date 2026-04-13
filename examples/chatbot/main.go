// Package main implements a reference HTTP chatbot server demonstrating
// the amp-go SDK end-to-end: auto-init via blank import, middleware chain
// (conversation → least-privilege), agent/LLM span instrumentation, and
// multi-turn conversation management.
//
// The chatbot talks to any OpenAI-compatible API endpoint (Ollama, AI Gateway,
// OpenAI) configured via LLM_API_URL.
//
// Usage:
//
//	export AMP_OTEL_ENDPOINT=http://obs-gateway:22893
//	export AMP_AGENT_API_KEY=<key>
//	export AMP_BASE_URL=http://amp-api:9095
//	export AMP_ORG_NAME=myorg
//	export AMP_PROJECT_NAME=myproj
//	export AMP_AGENT_NAME=chatbot
//	export LLM_API_URL=http://localhost:11434/v1   # Ollama
//	export LLM_MODEL=llama3.2
//	go run .
//
// Test:
//
//	curl -X POST http://localhost:8080/chat \
//	  -H "Content-Type: application/json" \
//	  -d '{"message": "Hello, what can you do?"}'
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wso2/amp-go/amp"
	"go.uber.org/zap"
)

func main() {
	// --- SDK init (explicit mode; for auto-init, use blank import instead) ---
	cfg := amp.FromEnv()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "Config error: %v\n", err)
		os.Exit(1)
	}

	client, err := amp.Init(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SDK init failed: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		client.Shutdown(ctx)
	}()

	logger := client.Logger()

	// --- Build middleware chain (BRD §10.2) ---
	//   ConversationMiddleware → LeastPrivilegeMiddleware → chatHandler
	//
	// Agent scopes: define what this agent is allowed to do.
	// In production, these come from agent registration metadata.
	agentScopes := parseAgentScopes()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatHandler(w, r, client, logger)
	})

	chain := client.ConversationMiddleware()(
		client.LeastPrivilegeMiddleware(agentScopes)(handler),
	)

	mux := http.NewServeMux()
	mux.Handle("/chat", chain)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// --- Start server ---
	addr := envDefault("LISTEN_ADDR", ":8080")
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("chatbot listening", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()

	// Graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh

	logger.Info("shutting down server")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// --- Chat handler (BRD §10.3) ---

type chatRequestBody struct {
	Message string `json:"message"`
}

type chatResponseBody struct {
	Reply          string       `json:"reply"`
	ConversationID string       `json:"conversation_id"`
	Tokens         tokenCounts  `json:"tokens"`
	LatencyMs      int64        `json:"latency_ms"`
}

type tokenCounts struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
}

func chatHandler(w http.ResponseWriter, r *http.Request, client *amp.AMPClient, logger *zap.Logger) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error": "method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()

	// Parse request body.
	var req chatRequestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error": "invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if req.Message == "" {
		http.Error(w, `{"error": "message field required"}`, http.StatusBadRequest)
		return
	}

	// Extract middleware-injected values.
	convID := amp.ConversationIDFromContext(r.Context())
	effectiveScope := amp.EffectiveScopeFromContext(r.Context())

	logger.Info("chat request",
		zap.String("conversation_id", convID),
		zap.String("message_preview", truncateStr(req.Message, 80)),
		zap.Strings("effective_scope", effectiveScope),
	)

	// Get or create conversation, retrieve history.
	conv, _ := client.Conversation.GetOrCreate(convID)
	history := conv.GetMessages()

	// Build LLM message list: system prompt + history + current message.
	cfg := client.Config()
	messages := buildLLMMessages(history, req.Message, effectiveScope)

	var llmResult amp.LLMResult

	// Wrap in AgentSpan → LLMSpan (BRD §10.3).
	agentErr := client.AgentSpan(r.Context(), cfg.AgentName, convID,
		[]amp.Message{{Role: "user", Content: req.Message}},
		func(ctx context.Context) error {
			var err error
			llmResult, err = client.LLMSpan(ctx, amp.LLMRequest{
				Model:       cfg.LLMModel,
				Provider:    detectProvider(cfg.LLMBaseURL),
				Temperature: 0.7,
				Messages:    messages,
			}, func(spanCtx context.Context) (amp.LLMResult, error) {
				return callLLM(spanCtx, client, messages)
			})
			return err
		},
	)

	if agentErr != nil {
		logger.Error("agent span error", zap.Error(agentErr))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": agentErr.Error()})
		return
	}

	// Update conversation history.
	conv.AppendExchange(req.Message, llmResult.Content)

	// Send response.
	latency := time.Since(start)
	resp := chatResponseBody{
		Reply:          llmResult.Content,
		ConversationID: convID,
		Tokens: tokenCounts{
			Prompt:     llmResult.PromptTokens,
			Completion: llmResult.CompletionTokens,
		},
		LatencyMs: latency.Milliseconds(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// --- LLM Backend (OpenAI-compatible) ---

// openAIChatRequest is the request body for the OpenAI /v1/chat/completions API.
type openAIChatRequest struct {
	Model       string        `json:"model"`
	Messages    []amp.Message `json:"messages"`
	Temperature float64       `json:"temperature"`
}

// openAIChatResponse is the response from /v1/chat/completions.
type openAIChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message      amp.Message `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func callLLM(ctx context.Context, client *amp.AMPClient, messages []amp.Message) (amp.LLMResult, error) {
	cfg := client.Config()

	reqBody := openAIChatRequest{
		Model:       cfg.LLMModel,
		Messages:    messages,
		Temperature: 0.7,
	}

	bodyBytes, _ := json.Marshal(reqBody)
	url := strings.TrimRight(cfg.LLMBaseURL, "/") + "/chat/completions"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return amp.LLMResult{}, fmt.Errorf("build LLM request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if cfg.LLMAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.LLMAPIKey)
	}

	// Inject W3C traceparent for distributed trace stitching.
	client.Tracer.InjectTraceparent(ctx, httpReq)

	httpClient := &http.Client{Timeout: 120 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return amp.LLMResult{}, fmt.Errorf("LLM call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return amp.LLMResult{}, fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(body))
	}

	var llmResp openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&llmResp); err != nil {
		return amp.LLMResult{}, fmt.Errorf("decode LLM response: %w", err)
	}

	content := ""
	finishReason := ""
	if len(llmResp.Choices) > 0 {
		content = llmResp.Choices[0].Message.Content
		finishReason = llmResp.Choices[0].FinishReason
	}

	return amp.LLMResult{
		Content:          content,
		PromptTokens:     llmResp.Usage.PromptTokens,
		CompletionTokens: llmResp.Usage.CompletionTokens,
		FinishReason:     finishReason,
		Model:            llmResp.Model,
	}, nil
}

// --- Helpers ---

func buildLLMMessages(history []amp.Message, currentMsg string, effectiveScope []string) []amp.Message {
	systemPrompt := "You are a helpful assistant."
	if len(effectiveScope) > 0 {
		systemPrompt += fmt.Sprintf(
			" You are operating with the following permissions: %s. Only provide information within these scopes.",
			strings.Join(effectiveScope, ", "),
		)
	}

	msgs := []amp.Message{
		{Role: "system", Content: systemPrompt},
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, amp.Message{Role: "user", Content: currentMsg})
	return msgs
}

func detectProvider(baseURL string) string {
	switch {
	case strings.Contains(baseURL, "ollama") || strings.Contains(baseURL, "11434"):
		return "ollama"
	case strings.Contains(baseURL, "openai.com"):
		return "openai"
	case strings.Contains(baseURL, "anthropic"):
		return "anthropic"
	default:
		return "openai" // Default to openai-compatible
	}
}

func parseAgentScopes() []string {
	raw := os.Getenv("AGENT_SCOPES")
	if raw == "" {
		return []string{"read:all"} // Default scope for the reference chatbot
	}
	return strings.Fields(raw)
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func truncateStr(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen-3] + "..."
	}
	return s
}
