package amp

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// testKey is a throwaway RSA key for signing test JWTs.
var testKey *rsa.PrivateKey

func init() {
	var err error
	testKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("failed to generate test RSA key: " + err.Error())
	}
}

// signTestJWT creates a signed JWT with the given claims.
func signTestJWT(t *testing.T, claims jwt.Claims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(testKey)
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signed
}

// signTestMapJWT creates a signed JWT with map claims.
func signTestMapJWT(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(testKey)
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signed
}

func TestParseAgentClaims(t *testing.T) {
	raw := signTestJWT(t, &agentTokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "test-agent",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		ComponentUID:   "comp-123",
		EnvironmentUID: "env-456",
		ProjectUID:     "proj-789",
	})

	claims, err := parseAgentClaims(raw)
	if err != nil {
		t.Fatalf("parseAgentClaims() error = %v", err)
	}

	if claims.ComponentUID != "comp-123" {
		t.Errorf("ComponentUID = %q, want %q", claims.ComponentUID, "comp-123")
	}
	if claims.EnvironmentUID != "env-456" {
		t.Errorf("EnvironmentUID = %q, want %q", claims.EnvironmentUID, "env-456")
	}
	if claims.ProjectUID != "proj-789" {
		t.Errorf("ProjectUID = %q, want %q", claims.ProjectUID, "proj-789")
	}
}

func TestParseScopesFromJWT(t *testing.T) {
	tests := []struct {
		name       string
		claims     jwt.MapClaims
		wantScopes []string
		wantErr    bool
	}{
		{
			name:       "space-delimited scopes (WSO2 IS format)",
			claims:     jwt.MapClaims{"scope": "read:docs write:orders admin:settings"},
			wantScopes: []string{"read:docs", "write:orders", "admin:settings"},
		},
		{
			name:       "single scope",
			claims:     jwt.MapClaims{"scope": "read:all"},
			wantScopes: []string{"read:all"},
		},
		{
			name:       "empty scope string",
			claims:     jwt.MapClaims{"scope": ""},
			wantScopes: nil,
		},
		{
			name:       "no scope claim",
			claims:     jwt.MapClaims{"sub": "user1"},
			wantScopes: nil,
		},
		{
			name:    "unsupported scope type",
			claims:  jwt.MapClaims{"scope": 42},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := signTestMapJWT(t, tt.claims)
			scopes, err := ParseScopesFromJWT(raw)

			if (err != nil) != tt.wantErr {
				t.Errorf("ParseScopesFromJWT() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if !slicesEqual(scopes, tt.wantScopes) {
					t.Errorf("ParseScopesFromJWT() = %v, want %v", scopes, tt.wantScopes)
				}
			}
		})
	}
}

func TestIntersectScopes(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		want []string
	}{
		{
			name: "partial overlap",
			a:    []string{"read:docs", "write:orders", "admin:settings"},
			b:    []string{"read:docs", "read:orders"},
			want: []string{"read:docs"},
		},
		{
			name: "full overlap",
			a:    []string{"read:docs", "write:docs"},
			b:    []string{"read:docs", "write:docs"},
			want: []string{"read:docs", "write:docs"},
		},
		{
			name: "no overlap",
			a:    []string{"admin:settings"},
			b:    []string{"read:docs"},
			want: nil,
		},
		{
			name: "a empty — returns b (fail-open for no employee constraint)",
			a:    nil,
			b:    []string{"read:docs"},
			want: []string{"read:docs"},
		},
		{
			name: "b empty — returns a",
			a:    []string{"read:docs"},
			b:    nil,
			want: []string{"read:docs"},
		},
		{
			name: "both empty",
			a:    nil,
			b:    nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IntersectScopes(tt.a, tt.b)
			if !slicesEqual(got, tt.want) {
				t.Errorf("IntersectScopes(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestTokenManagerAcquire(t *testing.T) {
	// Create a test JWT.
	expiry := time.Now().Add(1 * time.Hour)
	issued := time.Now()

	agentJWT := signTestJWT(t, &agentTokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "test-agent",
			ExpiresAt: jwt.NewNumericDate(expiry),
			IssuedAt:  jwt.NewNumericDate(issued),
		},
		ComponentUID:   "comp-test",
		EnvironmentUID: "env-test",
		ProjectUID:     "proj-test",
	})

	// Mock AMP API token endpoint.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}

		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-api-key" {
			t.Errorf("Authorization = %q, want %q", auth, "Bearer test-api-key")
		}

		resp := tokenResponse{
			Token:     agentJWT,
			ExpiresAt: expiry.Unix(),
			IssuedAt:  issued.Unix(),
			TokenType: "Bearer",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := Config{
		BaseURL:            server.URL,
		AgentAPIKey:        "test-api-key",
		OrgName:            "org",
		ProjectName:        "proj",
		AgentName:          "agent",
		TokenRefreshBuffer: 5 * time.Minute,
	}

	logger, _ := zap.NewDevelopment()
	tm, err := NewTokenManager(cfg, logger)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}
	defer tm.Shutdown()

	// Verify token was acquired.
	if tm.Token() != agentJWT {
		t.Errorf("Token() = %q..., want %q...", tm.Token()[:20], agentJWT[:20])
	}

	if tm.IsExpired() {
		t.Error("token should not be expired")
	}

	claims := tm.Claims()
	if claims == nil {
		t.Fatal("Claims() returned nil")
	}
	if claims.ComponentUID != "comp-test" {
		t.Errorf("ComponentUID = %q, want %q", claims.ComponentUID, "comp-test")
	}

	if tm.ExpiresAt().IsZero() {
		t.Error("ExpiresAt() returned zero time")
	}
	if tm.IssuedAt().IsZero() {
		t.Error("IssuedAt() returned zero time")
	}
}

func TestTokenManagerAcquireFailure(t *testing.T) {
	// Mock server that returns 401.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "invalid API key"}`))
	}))
	defer server.Close()

	cfg := Config{
		BaseURL:     server.URL,
		AgentAPIKey: "bad-key",
		OrgName:     "org",
		ProjectName: "proj",
		AgentName:   "agent",
	}

	logger, _ := zap.NewDevelopment()
	_, err := NewTokenManager(cfg, logger)
	if err == nil {
		t.Fatal("expected error from NewTokenManager with 401 response")
	}
}

// --- helpers ---

func slicesEqual(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
