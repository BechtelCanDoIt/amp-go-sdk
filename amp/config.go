// Package amp provides the core Go SDK for the WSO2 Agent Manager platform.
//
// amp-go tracks WSO2 Agent Manager v1.0.0 and is the Go counterpart of the
// Python amp-instrumentation package's manual path: InitOTel() wires the
// OTLP/HTTP exporter to AMP (the analog of init_otel()), and the span helpers
// emit spans that follow AMP's manual instrumentation contract (OpenTelemetry
// GenAI semantic conventions plus the OpenLLMetry traceloop.* extension keys).
// On top of that it offers optional agent JWT lifecycle management,
// least-privilege scope enforcement, and multi-turn conversation identity —
// all via environment-variable-driven configuration.
package amp

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Version is the WSO2 Agent Manager release this SDK tracks. It is reported
// as the instrumentation version on every span the SDK emits.
const Version = "1.0.0"

// otlpTracesPath is the OTLP/HTTP traces signal path appended to
// AMP_OTEL_ENDPOINT.
const otlpTracesPath = "/v1/traces"

// Config holds all AMP SDK configuration, populated from AMP_* environment
// variables. Required fields are validated at Init() time; missing required
// values produce a clear, actionable error.
type Config struct {
	// --- Required ---

	// OTELEndpoint is the AMP OTLP/HTTP base endpoint. Spans are POSTed to
	// <OTELEndpoint>/v1/traces.
	// Env: AMP_OTEL_ENDPOINT
	OTELEndpoint string

	// AgentAPIKey is the agent API key from AMP Console, sent as the
	// x-amp-api-key header.
	// Env: AMP_AGENT_API_KEY
	AgentAPIKey string

	// --- Optional: control plane ---
	//
	// Tracing needs none of these. When BaseURL, OrgName, ProjectName and
	// AgentName are all set, Init() also acquires and refreshes an agent JWT.

	// BaseURL is the AMP API base URL (agent-manager-service).
	// Env: AMP_BASE_URL
	BaseURL string

	// OrgName is the organization name in AMP.
	// Env: AMP_ORG_NAME
	OrgName string

	// ProjectName is the project name in AMP.
	// Env: AMP_PROJECT_NAME
	ProjectName string

	// AgentName is the agent name in AMP. Also used as the default
	// gen_ai.agent.name and as the service.name resource attribute.
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

	// Environment is the AMP environment name. It scopes agent JWT requests
	// and trace queries; when empty the AMP API uses its own default.
	// Env: AMP_ENVIRONMENT  Default: ""
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

	// TraceObserverURL is the agent-manager-observer base URL (used by the CLI
	// trace cmd).
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
		OTELEndpoint:       strings.TrimSpace(os.Getenv("AMP_OTEL_ENDPOINT")),
		AgentAPIKey:        strings.TrimSpace(os.Getenv("AMP_AGENT_API_KEY")),
		BaseURL:            os.Getenv("AMP_BASE_URL"),
		OrgName:            os.Getenv("AMP_ORG_NAME"),
		ProjectName:        os.Getenv("AMP_PROJECT_NAME"),
		AgentName:          os.Getenv("AMP_AGENT_NAME"),
		TraceContent:       envBool("AMP_TRACE_CONTENT", true),
		AgentVersion:       os.Getenv("AMP_AGENT_VERSION"),
		TokenRefreshBuffer: envDuration("AMP_TOKEN_REFRESH_BUFFER", 5*time.Minute),
		Debug:              envBool("AMP_DEBUG", false),
		Environment:        os.Getenv("AMP_ENVIRONMENT"),
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
	if strings.TrimSpace(c.OTELEndpoint) == "" {
		missing = append(missing, "AMP_OTEL_ENDPOINT")
	}
	if strings.TrimSpace(c.AgentAPIKey) == "" {
		missing = append(missing, "AMP_AGENT_API_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("amp: missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return nil
}

// ControlPlaneConfigured reports whether everything needed to call the AMP API
// on behalf of this agent (BaseURL, OrgName, ProjectName, AgentName) is set.
func (c Config) ControlPlaneConfigured() bool {
	return c.BaseURL != "" && c.OrgName != "" && c.ProjectName != "" && c.AgentName != ""
}

// OTLPTracesEndpoint returns the full OTLP/HTTP traces URL:
// <AMP_OTEL_ENDPOINT>/v1/traces. An endpoint that already ends in /v1/traces
// is returned as-is, and a scheme-less endpoint is treated as plain HTTP.
func (c Config) OTLPTracesEndpoint() string {
	base := strings.TrimRight(strings.TrimSpace(c.OTELEndpoint), "/")
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	if strings.HasSuffix(base, otlpTracesPath) {
		return base
	}
	return base + otlpTracesPath
}

// TokenEndpoint returns the full URL for the agent token acquisition endpoint.
func (c Config) TokenEndpoint() string {
	endpoint := fmt.Sprintf("%s/api/v1/orgs/%s/projects/%s/agents/%s/token",
		strings.TrimRight(c.BaseURL, "/"),
		url.PathEscape(c.OrgName),
		url.PathEscape(c.ProjectName),
		url.PathEscape(c.AgentName),
	)
	if c.Environment != "" {
		endpoint += "?environment=" + url.QueryEscape(c.Environment)
	}
	return endpoint
}

// TracesEndpoint returns the agent-manager-observer URL for querying traces,
// or "" when AMP_TRACE_OBSERVER_URL is not set.
func (c Config) TracesEndpoint() string {
	if c.TraceObserverURL == "" {
		return ""
	}
	return strings.TrimRight(c.TraceObserverURL, "/") + "/api/v1/traces"
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
