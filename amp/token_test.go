package amp

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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
		OrgID:          "org-321",
		Namespace:      "acme-dev",
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
	if claims.OrgID != "org-321" {
		t.Errorf("OrgID = %q, want %q", claims.OrgID, "org-321")
	}
	if claims.Namespace != "acme-dev" {
		t.Errorf("Namespace = %q, want %q", claims.Namespace, "acme-dev")
	}
}

func TestParseAgentClaims_Malformed(t *testing.T) {
	if _, err := parseAgentClaims("not-a-jwt"); err == nil {
		t.Error("parseAgentClaims() should reject a malformed token")
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

func TestTokenManagerAcquire_BadResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"invalid JSON", `not json`},
		{"empty token", `{"token": "", "expires_at": 1, "issued_at": 1}`},
		{"token is not a JWT", `{"token": "not-a-jwt", "expires_at": 1, "issued_at": 1}`},
	}

	logger, _ := zap.NewDevelopment()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tt.body))
			}))
			defer server.Close()

			_, err := NewTokenManager(Config{
				BaseURL: server.URL, AgentAPIKey: "key",
				OrgName: "org", ProjectName: "proj", AgentName: "agent",
			}, logger)
			if err == nil {
				t.Fatal("expected NewTokenManager to fail")
			}
		})
	}
}

func TestTokenManager_ZeroValue(t *testing.T) {
	// Before any token is stored the accessors must be safe and report "no token".
	tm := &TokenManager{}
	if tm.Token() != "" {
		t.Errorf("Token() = %q, want empty", tm.Token())
	}
	if tm.Claims() != nil {
		t.Error("Claims() should be nil")
	}
	if !tm.IsExpired() {
		t.Error("IsExpired() should be true without a token")
	}
	if !tm.ExpiresAt().IsZero() || !tm.IssuedAt().IsZero() {
		t.Error("ExpiresAt()/IssuedAt() should be zero without a token")
	}
	tm.Shutdown() // no refresh goroutine — must not panic
}

func TestTokenManager_ExpiredToken(t *testing.T) {
	expiry := time.Now().Add(-time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(tokenResponse{
			Token: signTestJWT(t, &agentTokenClaims{
				RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(expiry)},
			}),
			ExpiresAt: expiry.Unix(),
		})
	}))
	defer server.Close()

	logger, _ := zap.NewDevelopment()
	tm, err := NewTokenManager(Config{
		BaseURL: server.URL, AgentAPIKey: "key",
		OrgName: "org", ProjectName: "proj", AgentName: "agent",
	}, logger)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}
	if !tm.IsExpired() {
		t.Error("IsExpired() should be true for a token past its expiry")
	}
}

func TestRefreshAt(t *testing.T) {
	acquired := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		lifetime time.Duration
		buffer   time.Duration
		want     time.Duration // after acquisition
	}{
		{"buffer before expiry", time.Hour, 5 * time.Minute, 55 * time.Minute},
		{"zero buffer waits for expiry", time.Hour, 0, time.Hour},
		{"buffer equal to lifetime refreshes at half-life", time.Hour, time.Hour, 30 * time.Minute},
		{"buffer longer than lifetime refreshes at half-life", 4 * time.Minute, 5 * time.Minute, 2 * time.Minute},
		{"very short token waits the minimum interval", time.Second, 5 * time.Minute, minRefreshInterval},
		{"already expired token waits the minimum interval", -time.Minute, 5 * time.Minute, minRefreshInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &tokenState{AcquiredAt: acquired, ExpiresAt: acquired.Add(tt.lifetime)}
			if got := st.refreshAt(tt.buffer).Sub(acquired); got != tt.want {
				t.Errorf("refresh due %s after acquisition, want %s", got, tt.want)
			}
		})
	}
}

// TestTokenManagerRefresh_NoHotLoop guards against the refresh loop spinning
// when every freshly issued token is already inside the refresh window.
func TestTokenManagerRefresh_NoHotLoop(t *testing.T) {
	tests := []struct {
		name     string
		lifetime time.Duration
		buffer   time.Duration
	}{
		{"buffer longer than token lifetime", time.Hour, 2 * time.Hour},
		{"AMP API issues already-expired tokens", -time.Minute, 5 * time.Minute},
	}

	logger := zap.NewNop()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				expiry := time.Now().Add(tt.lifetime)
				json.NewEncoder(w).Encode(tokenResponse{
					Token: signTestJWT(t, &agentTokenClaims{
						RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(expiry)},
					}),
					ExpiresAt: expiry.Unix(),
				})
			}))
			defer server.Close()

			tm, err := NewTokenManager(Config{
				BaseURL: server.URL, AgentAPIKey: "key",
				OrgName: "org", ProjectName: "proj", AgentName: "agent",
				TokenRefreshBuffer: tt.buffer,
			}, logger)
			if err != nil {
				t.Fatalf("NewTokenManager() error = %v", err)
			}
			tm.StartRefresh()
			defer tm.Shutdown()

			// Well inside minRefreshInterval: only the initial acquisition
			// may have happened.
			time.Sleep(minRefreshInterval / 2)
			if n := calls.Load(); n != 1 {
				t.Errorf("%d token requests in %s, want 1", n, minRefreshInterval/2)
			}
		})
	}
}

// TestTokenManagerRefresh covers the background lifecycle: the token is
// refreshed before it expires, a failed refresh is retried with backoff, and
// Shutdown stops the loop.
func TestTokenManagerRefresh(t *testing.T) {
	var (
		mu     sync.Mutex
		calls  int
		tokens []string
		times  []time.Time // arrival time of each token request
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		times = append(times, time.Now())
		mu.Unlock()

		// Call 1: a short-lived token, so a refresh is due almost at once.
		// Call 2: the refresh fails. Call 3+: a long-lived token.
		if n == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		expiry := time.Now().Add(2 * time.Second)
		if n > 2 {
			expiry = time.Now().Add(time.Hour)
		}
		raw := signTestJWT(t, &agentTokenClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				ID:        fmt.Sprintf("token-%d", n),
				ExpiresAt: jwt.NewNumericDate(expiry),
			},
		})
		mu.Lock()
		tokens = append(tokens, raw)
		mu.Unlock()
		json.NewEncoder(w).Encode(tokenResponse{Token: raw, ExpiresAt: expiry.Unix(), IssuedAt: time.Now().Unix()})
	}))
	defer server.Close()

	logger, _ := zap.NewDevelopment()
	tm, err := NewTokenManager(Config{
		BaseURL: server.URL, AgentAPIKey: "key",
		OrgName: "org", ProjectName: "proj", AgentName: "agent",
		// Refresh 1s before expiry: due at once for the 2s token, about an
		// hour away for its long-lived replacement.
		TokenRefreshBuffer: time.Second,
	}, logger)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}
	first := tm.Token()
	tm.StartRefresh()

	// Failed refresh (call 2) backs off 1s, then call 3 succeeds.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(tokens) >= 2
		mu.Unlock()
		if done && tm.Token() != first {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	if len(tokens) < 2 {
		mu.Unlock()
		t.Fatalf("token was not refreshed after a failed attempt (calls=%d)", calls)
	}
	second := tokens[1]
	retryGap := times[2].Sub(times[1])
	mu.Unlock()

	// The retry after the failed refresh waits out the 1s backoff rather than
	// hammering the AMP API.
	if retryGap < 900*time.Millisecond {
		t.Errorf("retry came %s after the failed refresh, want ~1s backoff", retryGap)
	}

	if tm.Token() != second {
		t.Error("Token() should return the refreshed token")
	}
	if tm.IsExpired() {
		t.Error("refreshed token should not be expired")
	}
	if id := tm.Claims().ID; id != "token-3" {
		t.Errorf("claims ID = %q, want token-3", id)
	}

	// After Shutdown the loop makes no further requests.
	tm.Shutdown()
	tm.Shutdown() // safe to call twice
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	before := calls
	mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	after := calls
	mu.Unlock()
	if after != before {
		t.Errorf("refresh loop still running after Shutdown: %d → %d calls", before, after)
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
