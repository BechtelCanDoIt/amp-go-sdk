// Package amp provides the core Go SDK for the WSO2 Agent Manager platform.
//
// amp-go is functionally equivalent to the Python amp-instrumentation SDK,
// providing agent JWT lifecycle management, OpenTelemetry span instrumentation
// (agent/llm/tool), least-privilege scope enforcement, and multi-turn
// conversation identity — all via environment-variable-driven configuration.
package amp

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all AMP SDK configuration, populated from AMP_* environment
// variables. Required fields are validated at Init() time; missing required
// values produce a clear, actionable error.
type Config struct {
	// --- Required ---

	// OTELEndpoint is the OTLP HTTP endpoint (obs-gateway).
	// Env: AMP_OTEL_ENDPOINT
	OTELEndpoint string

	// AgentAPIKey is the agent API key from AMP Console.
	// Env: AMP_AGENT_API_KEY
	AgentAPIKey string

	// BaseURL is the AMP API base URL (agent-manager-service).
	// Env: AMP_BASE_URL
	BaseURL string

	// OrgName is the organization name in AMP.
	// Env: AMP_ORG_NAME
	OrgName string

	// ProjectName is the project name in AMP.
	// Env: AMP_PROJECT_NAME
	ProjectName string

	// AgentName is the agent name in AMP.
	// Env: AMP_AGENT_NAME
	AgentName string

	// --- Optional ---

	// TraceContent controls whether LLM prompts/completions are captured.
	// Env: AMP_TRACE_CONTENT  Default: true
	TraceContent bool

	// AgentVersion is set as the agent-manager/agent-version resource attribute.
	// Env: AMP_AGENT_VERSION  Default: ""
	AgentVersion string

	// TokenRefreshBuffer is how far before JWT expiry the refresh goroutine
	// wakes to acquire a new token.
	// Env: AMP_TOKEN_REFRESH_BUFFER  Default: 5m
	TokenRefreshBuffer time.Duration

	// Debug enables verbose SDK logging.
	// Env: AMP_DEBUG  Default: false
	Debug bool

	// Environment is the target environment name for trace enrichment.
	// Env: AMP_ENVIRONMENT  Default: "development"
	Environment string

	// LLMBaseURL is the LLM backend base URL (OpenAI-compatible).
	// Env: LLM_API_URL  Default: "http://localhost:11434/v1" (Ollama)
	LLMBaseURL string

	// LLMAPIKey is the API key for the LLM backend.
	// Env: LLM_API_KEY  Default: "" (Ollama needs none)
	LLMAPIKey string

	// LLMModel is the default model to use for LLM calls.
	// Env: LLM_MODEL  Default: "llama3.2"
	LLMModel string

	// TraceObserverURL is the amp-trace-observer endpoint (used by CLI trace cmd).
	// If empty, defaults to BaseURL proxy path.
	// Env: AMP_TRACE_OBSERVER_URL
	TraceObserverURL string

	// ConversationTTL is how long idle conversations are kept in memory.
	// Default: 30m
	ConversationTTL time.Duration
}

// FromEnv reads configuration from environment variables. It does NOT validate
// required fields — call Validate() or let Init() do it.
func FromEnv() Config {
	return Config{
		OTELEndpoint:       os.Getenv("AMP_OTEL_ENDPOINT"),
		AgentAPIKey:        os.Getenv("AMP_AGENT_API_KEY"),
		BaseURL:            os.Getenv("AMP_BASE_URL"),
		OrgName:            os.Getenv("AMP_ORG_NAME"),
		ProjectName:        os.Getenv("AMP_PROJECT_NAME"),
		AgentName:          os.Getenv("AMP_AGENT_NAME"),
		TraceContent:       envBool("AMP_TRACE_CONTENT", true),
		AgentVersion:       os.Getenv("AMP_AGENT_VERSION"),
		TokenRefreshBuffer: envDuration("AMP_TOKEN_REFRESH_BUFFER", 5*time.Minute),
		Debug:              envBool("AMP_DEBUG", false),
		Environment:        envString("AMP_ENVIRONMENT", "development"),
		LLMBaseURL:         envString("LLM_API_URL", "http://localhost:11434/v1"),
		LLMAPIKey:          os.Getenv("LLM_API_KEY"),
		LLMModel:           envString("LLM_MODEL", "llama3.2"),
		TraceObserverURL:   os.Getenv("AMP_TRACE_OBSERVER_URL"),
		ConversationTTL:    envDuration("AMP_CONVERSATION_TTL", 30*time.Minute),
	}
}

// Validate checks that all required fields are present and returns a
// descriptive error listing every missing field.
func (c Config) Validate() error {
	var missing []string
	if c.OTELEndpoint == "" {
		missing = append(missing, "AMP_OTEL_ENDPOINT")
	}
	if c.AgentAPIKey == "" {
		missing = append(missing, "AMP_AGENT_API_KEY")
	}
	if c.BaseURL == "" {
		missing = append(missing, "AMP_BASE_URL")
	}
	if c.OrgName == "" {
		missing = append(missing, "AMP_ORG_NAME")
	}
	if c.ProjectName == "" {
		missing = append(missing, "AMP_PROJECT_NAME")
	}
	if c.AgentName == "" {
		missing = append(missing, "AMP_AGENT_NAME")
	}
	if len(missing) > 0 {
		return fmt.Errorf("amp: missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return nil
}

// TokenEndpoint returns the full URL for the agent token acquisition endpoint.
func (c Config) TokenEndpoint() string {
	return fmt.Sprintf("%s/api/v1/orgs/%s/projects/%s/agents/%s/token",
		strings.TrimRight(c.BaseURL, "/"),
		c.OrgName,
		c.ProjectName,
		c.AgentName,
	)
}

// TracesEndpoint returns the AMP API proxy URL for querying traces.
func (c Config) TracesEndpoint() string {
	if c.TraceObserverURL != "" {
		return strings.TrimRight(c.TraceObserverURL, "/") + "/api/v1/traces"
	}
	return fmt.Sprintf("%s/api/v1/orgs/%s/projects/%s/agents/%s/traces",
		strings.TrimRight(c.BaseURL, "/"),
		c.OrgName,
		c.ProjectName,
		c.AgentName,
	)
}

// --- helpers ---

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	// Try Go duration format first ("5m", "30s", etc.)
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	// Try plain seconds
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	return fallback
}
