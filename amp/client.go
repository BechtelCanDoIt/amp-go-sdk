package amp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// AMPClient is the top-level SDK entry point. It wires together OTLP tracing,
// conversation management, HTTP middleware, and (optionally) JWT lifecycle.
type AMPClient struct {
	cfg    Config
	logger *zap.Logger

	// Token is nil unless the control plane is configured
	// (see Config.ControlPlaneConfigured).
	Token        *TokenManager
	Tracer       *TracerManager
	Conversation *ConversationManager
}

// Init creates and fully initializes an AMPClient:
//  1. Validates configuration (AMP_OTEL_ENDPOINT and AMP_AGENT_API_KEY).
//  2. Configures the OTLP exporter and TracerProvider.
//  3. If the control plane is configured, acquires the initial agent JWT and
//     starts the background refresh goroutine.
//  4. Starts the conversation TTL eviction goroutine.
func Init(cfg Config) (*AMPClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Set up structured logger.
	var logger *zap.Logger
	var err error
	if cfg.Debug {
		logger, err = zap.NewDevelopment()
	} else {
		logger, err = zap.NewProduction()
	}
	if err != nil {
		return nil, fmt.Errorf("amp: create logger: %w", err)
	}
	logger = logger.Named("amp")

	logger.Info("initializing amp-go SDK",
		zap.String("version", Version),
		zap.String("agent", cfg.AgentName),
		zap.String("org", cfg.OrgName),
		zap.String("project", cfg.ProjectName),
		zap.String("environment", cfg.Environment),
	)

	// 1. Token manager — optional; acquire initial JWT.
	var tokenMgr *TokenManager
	if cfg.ControlPlaneConfigured() {
		tokenMgr, err = NewTokenManager(cfg, logger)
		if err != nil {
			return nil, err
		}
	} else {
		logger.Debug("control plane not configured — agent JWT management disabled")
	}

	// 2. Tracer manager — OTLP exporter + TracerProvider.
	tracerMgr, err := NewTracerManager(cfg, logger)
	if err != nil {
		if tokenMgr != nil {
			tokenMgr.Shutdown()
		}
		return nil, fmt.Errorf("amp: %w", err)
	}

	// 3. Start background refresh.
	if tokenMgr != nil {
		tokenMgr.StartRefresh()
	}

	// 4. Conversation manager.
	convMgr := NewConversationManager(cfg.ConversationTTL, logger)

	client := &AMPClient{
		cfg:          cfg,
		logger:       logger,
		Token:        tokenMgr,
		Tracer:       tracerMgr,
		Conversation: convMgr,
	}

	logger.Info("amp-go SDK initialized")
	return client, nil
}

// Shutdown flushes OTLP spans, stops the JWT refresh goroutine, stops
// conversation eviction, and releases all resources. Call this on SIGTERM/SIGINT.
func (c *AMPClient) Shutdown(ctx context.Context) error {
	c.logger.Info("shutting down amp-go SDK")

	if c.Token != nil {
		c.Token.Shutdown()
	}
	c.Conversation.Shutdown()

	if err := c.Tracer.Shutdown(ctx); err != nil {
		c.logger.Error("tracer shutdown error", zap.Error(err))
		return err
	}

	c.logger.Sync()
	return nil
}

// Config returns the active configuration (read-only).
func (c *AMPClient) Config() Config {
	return c.cfg
}

// Logger returns the SDK's logger for use in agent code.
func (c *AMPClient) Logger() *zap.Logger {
	return c.logger
}

// --- Convenience span methods (delegate to TracerManager) ---

// AgentSpan creates a root agent span. See TracerManager.AgentSpan for details.
func (c *AMPClient) AgentSpan(
	ctx context.Context,
	req AgentRequest,
	fn func(context.Context) (AgentResult, error),
) (AgentResult, error) {
	return c.Tracer.AgentSpan(ctx, req, fn)
}

// ChainSpan creates a workflow span. See TracerManager.ChainSpan for details.
func (c *AMPClient) ChainSpan(
	ctx context.Context,
	name string,
	input any,
	fn func(context.Context) (any, error),
) (any, error) {
	return c.Tracer.ChainSpan(ctx, name, input, fn)
}

// LLMSpan creates an LLM child span. See TracerManager.LLMSpan for details.
func (c *AMPClient) LLMSpan(
	ctx context.Context,
	req LLMRequest,
	fn func(context.Context) (LLMResult, error),
) (LLMResult, error) {
	return c.Tracer.LLMSpan(ctx, req, fn)
}

// EmbeddingSpan creates an embedding span. See TracerManager.EmbeddingSpan for
// details.
func (c *AMPClient) EmbeddingSpan(
	ctx context.Context,
	req EmbeddingRequest,
	fn func(context.Context) (EmbeddingResult, error),
) (EmbeddingResult, error) {
	return c.Tracer.EmbeddingSpan(ctx, req, fn)
}

// RetrieverSpan creates a retrieval span. See TracerManager.RetrieverSpan for
// details.
func (c *AMPClient) RetrieverSpan(
	ctx context.Context,
	req RetrieverRequest,
	fn func(context.Context) error,
) error {
	return c.Tracer.RetrieverSpan(ctx, req, fn)
}

// RerankSpan creates a rerank span. See TracerManager.RerankSpan for details.
func (c *AMPClient) RerankSpan(
	ctx context.Context,
	req RerankRequest,
	fn func(context.Context) error,
) error {
	return c.Tracer.RerankSpan(ctx, req, fn)
}

// ToolSpan creates a tool child span. See TracerManager.ToolSpan for details.
func (c *AMPClient) ToolSpan(
	ctx context.Context,
	req ToolRequest,
	fn func(context.Context) (any, error),
) (any, error) {
	return c.Tracer.ToolSpan(ctx, req, fn)
}

// --- Convenience middleware methods ---

// LeastPrivilegeMiddleware returns the scope-intersection middleware.
// agentScopes defines what the agent is allowed to do; these are intersected
// with the employee's scopes from X-User-Token.
func (c *AMPClient) LeastPrivilegeMiddleware(agentScopes []string) func(http.Handler) http.Handler {
	return LeastPrivilegeMiddleware(agentScopes, c.logger)
}

// ConversationMiddleware returns the conversation identity middleware.
func (c *AMPClient) ConversationMiddleware() func(http.Handler) http.Handler {
	return ConversationMiddleware(c.Conversation, c.logger)
}

// --- Health check ---

// HealthStatus represents the connectivity status of a single upstream service.
type HealthStatus struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
	Latency string `json:"latency,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Health checks connectivity to the AMP OTLP endpoint and, when the control
// plane is configured, the AMP API and agent JWT freshness.
func (c *AMPClient) Health(ctx context.Context) []HealthStatus {
	client := &http.Client{Timeout: 10 * time.Second}
	type check struct {
		name string
		url  string
	}
	// The OTLP endpoint only accepts POST; any response below 500 proves the
	// gateway is reachable.
	checks := []check{{"AMP OTLP endpoint", c.cfg.OTLPTracesEndpoint()}}
	if c.cfg.BaseURL != "" {
		checks = append(checks, check{"AMP API", strings.TrimRight(c.cfg.BaseURL, "/") + "/health"})
	}

	results := make([]HealthStatus, 0, len(checks)+1)
	for _, check := range checks {
		hs := HealthStatus{Name: check.name, URL: check.url}
		start := time.Now()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, check.url, nil)
		if err != nil {
			hs.Error = err.Error()
			results = append(results, hs)
			continue
		}

		resp, err := client.Do(req)
		hs.Latency = time.Since(start).String()
		if err != nil {
			hs.Error = err.Error()
		} else {
			resp.Body.Close()
			hs.Healthy = resp.StatusCode < 500
		}
		results = append(results, hs)
	}

	// Token freshness check.
	if c.Token != nil {
		tokenStatus := HealthStatus{
			Name:    "Agent JWT",
			Healthy: !c.Token.IsExpired(),
		}
		if c.Token.IsExpired() {
			tokenStatus.Error = "token expired"
		} else {
			st := c.Token.state()
			if st != nil {
				tokenStatus.Latency = fmt.Sprintf("expires in %s", time.Until(st.ExpiresAt).Round(time.Second))
			}
		}
		results = append(results, tokenStatus)
	}

	return results
}
