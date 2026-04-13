package amp

import (
	"os"
	"testing"
	"time"
)

func TestFromEnv(t *testing.T) {
	// Set all env vars.
	envs := map[string]string{
		"AMP_OTEL_ENDPOINT":       "http://obs-gateway:22893",
		"AMP_AGENT_API_KEY":       "test-api-key",
		"AMP_BASE_URL":            "http://amp-api:9095",
		"AMP_ORG_NAME":            "testorg",
		"AMP_PROJECT_NAME":        "testproj",
		"AMP_AGENT_NAME":          "testagent",
		"AMP_TRACE_CONTENT":       "false",
		"AMP_AGENT_VERSION":       "1.2.3",
		"AMP_TOKEN_REFRESH_BUFFER": "10m",
		"AMP_DEBUG":               "1",
		"AMP_ENVIRONMENT":         "staging",
		"LLM_API_URL":             "http://ollama:11434/v1",
		"LLM_API_KEY":             "sk-test",
		"LLM_MODEL":               "gpt-4o",
	}
	for k, v := range envs {
		os.Setenv(k, v)
	}
	defer func() {
		for k := range envs {
			os.Unsetenv(k)
		}
	}()

	cfg := FromEnv()

	if cfg.OTELEndpoint != "http://obs-gateway:22893" {
		t.Errorf("OTELEndpoint = %q, want %q", cfg.OTELEndpoint, "http://obs-gateway:22893")
	}
	if cfg.AgentAPIKey != "test-api-key" {
		t.Errorf("AgentAPIKey = %q, want %q", cfg.AgentAPIKey, "test-api-key")
	}
	if cfg.TraceContent != false {
		t.Errorf("TraceContent = %v, want false", cfg.TraceContent)
	}
	if cfg.AgentVersion != "1.2.3" {
		t.Errorf("AgentVersion = %q, want %q", cfg.AgentVersion, "1.2.3")
	}
	if cfg.TokenRefreshBuffer != 10*time.Minute {
		t.Errorf("TokenRefreshBuffer = %v, want 10m", cfg.TokenRefreshBuffer)
	}
	if cfg.Debug != true {
		t.Errorf("Debug = %v, want true", cfg.Debug)
	}
	if cfg.Environment != "staging" {
		t.Errorf("Environment = %q, want %q", cfg.Environment, "staging")
	}
	if cfg.LLMBaseURL != "http://ollama:11434/v1" {
		t.Errorf("LLMBaseURL = %q, want %q", cfg.LLMBaseURL, "http://ollama:11434/v1")
	}
	if cfg.LLMModel != "gpt-4o" {
		t.Errorf("LLMModel = %q, want %q", cfg.LLMModel, "gpt-4o")
	}
}

func TestFromEnvDefaults(t *testing.T) {
	// Clear all AMP env vars to test defaults.
	keys := []string{
		"AMP_TRACE_CONTENT", "AMP_AGENT_VERSION", "AMP_TOKEN_REFRESH_BUFFER",
		"AMP_DEBUG", "AMP_ENVIRONMENT", "LLM_API_URL", "LLM_API_KEY", "LLM_MODEL",
		"AMP_CONVERSATION_TTL",
	}
	for _, k := range keys {
		os.Unsetenv(k)
	}

	cfg := FromEnv()

	if cfg.TraceContent != true {
		t.Errorf("default TraceContent = %v, want true", cfg.TraceContent)
	}
	if cfg.TokenRefreshBuffer != 5*time.Minute {
		t.Errorf("default TokenRefreshBuffer = %v, want 5m", cfg.TokenRefreshBuffer)
	}
	if cfg.Debug != false {
		t.Errorf("default Debug = %v, want false", cfg.Debug)
	}
	if cfg.Environment != "development" {
		t.Errorf("default Environment = %q, want %q", cfg.Environment, "development")
	}
	if cfg.LLMBaseURL != "http://localhost:11434/v1" {
		t.Errorf("default LLMBaseURL = %q, want %q", cfg.LLMBaseURL, "http://localhost:11434/v1")
	}
	if cfg.LLMModel != "llama3.2" {
		t.Errorf("default LLMModel = %q, want %q", cfg.LLMModel, "llama3.2")
	}
	if cfg.ConversationTTL != 30*time.Minute {
		t.Errorf("default ConversationTTL = %v, want 30m", cfg.ConversationTTL)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "all required present",
			cfg: Config{
				OTELEndpoint: "http://obs:22893",
				AgentAPIKey:  "key",
				BaseURL:      "http://amp:9095",
				OrgName:      "org",
				ProjectName:  "proj",
				AgentName:    "agent",
			},
			wantErr: false,
		},
		{
			name:    "all missing",
			cfg:     Config{},
			wantErr: true,
			errMsg:  "AMP_OTEL_ENDPOINT",
		},
		{
			name: "partial missing",
			cfg: Config{
				OTELEndpoint: "http://obs:22893",
				AgentAPIKey:  "key",
			},
			wantErr: true,
			errMsg:  "AMP_BASE_URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && err != nil {
				if tt.errMsg != "" && !contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error = %q, should contain %q", err.Error(), tt.errMsg)
				}
			}
		})
	}
}

func TestTokenEndpoint(t *testing.T) {
	cfg := Config{
		BaseURL:     "http://amp-api:9095",
		OrgName:     "myorg",
		ProjectName: "myproj",
		AgentName:   "myagent",
	}

	got := cfg.TokenEndpoint()
	want := "http://amp-api:9095/api/v1/orgs/myorg/projects/myproj/agents/myagent/token"
	if got != want {
		t.Errorf("TokenEndpoint() = %q, want %q", got, want)
	}
}

func TestTokenEndpointTrailingSlash(t *testing.T) {
	cfg := Config{
		BaseURL:     "http://amp-api:9095/",
		OrgName:     "org",
		ProjectName: "proj",
		AgentName:   "agent",
	}

	got := cfg.TokenEndpoint()
	want := "http://amp-api:9095/api/v1/orgs/org/projects/proj/agents/agent/token"
	if got != want {
		t.Errorf("TokenEndpoint() = %q, want %q", got, want)
	}
}

func TestTracesEndpointProxy(t *testing.T) {
	cfg := Config{
		BaseURL:     "http://amp-api:9095",
		OrgName:     "org",
		ProjectName: "proj",
		AgentName:   "agent",
	}

	got := cfg.TracesEndpoint()
	want := "http://amp-api:9095/api/v1/orgs/org/projects/proj/agents/agent/traces"
	if got != want {
		t.Errorf("TracesEndpoint() = %q, want %q", got, want)
	}
}

func TestTracesEndpointDirect(t *testing.T) {
	cfg := Config{
		BaseURL:          "http://amp-api:9095",
		OrgName:          "org",
		ProjectName:      "proj",
		AgentName:        "agent",
		TraceObserverURL: "http://trace-observer:9098",
	}

	got := cfg.TracesEndpoint()
	want := "http://trace-observer:9098/api/v1/traces"
	if got != want {
		t.Errorf("TracesEndpoint() = %q, want %q", got, want)
	}
}

func TestEnvBool(t *testing.T) {
	tests := []struct {
		value    string
		fallback bool
		want     bool
	}{
		{"true", false, true},
		{"TRUE", false, true},
		{"1", false, true},
		{"yes", false, true},
		{"on", false, true},
		{"false", true, false},
		{"0", true, false},
		{"no", true, false},
		{"off", true, false},
		{"", true, true},
		{"", false, false},
		{"garbage", true, true},
	}

	for _, tt := range tests {
		os.Setenv("TEST_BOOL", tt.value)
		got := envBool("TEST_BOOL", tt.fallback)
		if got != tt.want {
			t.Errorf("envBool(%q, %v) = %v, want %v", tt.value, tt.fallback, got, tt.want)
		}
	}
	os.Unsetenv("TEST_BOOL")
}

func TestEnvDuration(t *testing.T) {
	tests := []struct {
		value    string
		fallback time.Duration
		want     time.Duration
	}{
		{"5m", time.Minute, 5 * time.Minute},
		{"30s", time.Minute, 30 * time.Second},
		{"120", time.Minute, 120 * time.Second}, // plain seconds
		{"", 5 * time.Minute, 5 * time.Minute},
		{"garbage", 5 * time.Minute, 5 * time.Minute},
	}

	for _, tt := range tests {
		os.Setenv("TEST_DUR", tt.value)
		got := envDuration("TEST_DUR", tt.fallback)
		if got != tt.want {
			t.Errorf("envDuration(%q, %v) = %v, want %v", tt.value, tt.fallback, got, tt.want)
		}
	}
	os.Unsetenv("TEST_DUR")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
