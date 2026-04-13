package amp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// tokenResponse matches the agent-manager-service TokenResponse struct.
// See: agent-manager-service/spec/token.go
type tokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	IssuedAt  int64  `json:"issued_at"`
	TokenType string `json:"token_type"`
}

// tokenRequest is the optional request body for token acquisition.
type tokenRequest struct {
	ExpiresIn string `json:"expires_in,omitempty"`
}

// agentTokenClaims mirrors the agent-manager AgentTokenClaims struct.
// The agent JWT carries platform identifiers, not scopes.
type agentTokenClaims struct {
	jwt.RegisteredClaims
	ComponentUID   string `json:"component_uid"`
	EnvironmentUID string `json:"environment_uid"`
	ProjectUID     string `json:"project_uid,omitempty"`
}

// tokenState is stored in an atomic.Value for lock-free concurrent access.
type tokenState struct {
	Raw       string
	ExpiresAt time.Time
	IssuedAt  time.Time
	Claims    *agentTokenClaims
}

// TokenManager handles JWT acquisition, atomic storage, and background refresh.
type TokenManager struct {
	cfg    Config
	logger *zap.Logger
	client *http.Client

	// current holds *tokenState; accessed atomically from all goroutines.
	current atomic.Value

	cancelRefresh context.CancelFunc
}

// NewTokenManager creates a TokenManager and acquires the initial token.
func NewTokenManager(cfg Config, logger *zap.Logger) (*TokenManager, error) {
	tm := &TokenManager{
		cfg:    cfg,
		logger: logger.Named("token"),
		client: &http.Client{Timeout: 30 * time.Second},
	}

	// Acquire initial token — fail fast if the AMP API is unreachable.
	if err := tm.acquire(); err != nil {
		return nil, fmt.Errorf("amp: initial token acquisition failed: %w", err)
	}

	return tm, nil
}

// StartRefresh launches the background refresh goroutine. Call this after
// successful initial acquisition. The goroutine is stopped by Shutdown().
func (tm *TokenManager) StartRefresh() {
	ctx, cancel := context.WithCancel(context.Background())
	tm.cancelRefresh = cancel
	go tm.refreshLoop(ctx)
}

// Shutdown stops the refresh goroutine. Safe to call multiple times.
func (tm *TokenManager) Shutdown() {
	if tm.cancelRefresh != nil {
		tm.cancelRefresh()
	}
}

// Token returns the current JWT string. Lock-free, safe for concurrent use.
func (tm *TokenManager) Token() string {
	st := tm.state()
	if st == nil {
		return ""
	}
	return st.Raw
}

// Claims returns the parsed claims from the current token.
func (tm *TokenManager) Claims() *agentTokenClaims {
	st := tm.state()
	if st == nil {
		return nil
	}
	return st.Claims
}

// IsExpired returns true if the current token has expired.
func (tm *TokenManager) IsExpired() bool {
	st := tm.state()
	if st == nil {
		return true
	}
	return time.Now().After(st.ExpiresAt)
}

// ExpiresAt returns the token expiration time. Returns zero time if no token.
func (tm *TokenManager) ExpiresAt() time.Time {
	st := tm.state()
	if st == nil {
		return time.Time{}
	}
	return st.ExpiresAt
}

// IssuedAt returns the token issuance time. Returns zero time if no token.
func (tm *TokenManager) IssuedAt() time.Time {
	st := tm.state()
	if st == nil {
		return time.Time{}
	}
	return st.IssuedAt
}

// state loads the current tokenState from atomic storage.
func (tm *TokenManager) state() *tokenState {
	v := tm.current.Load()
	if v == nil {
		return nil
	}
	return v.(*tokenState)
}

// acquire fetches a new token from the AMP API and stores it atomically.
func (tm *TokenManager) acquire() error {
	url := tm.cfg.TokenEndpoint()
	tm.logger.Debug("acquiring token", zap.String("url", url))

	body, _ := json.Marshal(tokenRequest{})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tm.cfg.AgentAPIKey)

	resp, err := tm.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("POST %s returned %d: %s", url, resp.StatusCode, string(respBody))
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}

	if tr.Token == "" {
		return fmt.Errorf("empty token in response from %s", url)
	}

	// Parse claims without signature verification — we trust the AMP API response.
	claims, err := parseAgentClaims(tr.Token)
	if err != nil {
		return fmt.Errorf("parse agent JWT claims: %w", err)
	}

	st := &tokenState{
		Raw:       tr.Token,
		ExpiresAt: time.Unix(tr.ExpiresAt, 0),
		IssuedAt:  time.Unix(tr.IssuedAt, 0),
		Claims:    claims,
	}
	tm.current.Store(st)

	tm.logger.Info("token acquired",
		zap.Time("expires_at", st.ExpiresAt),
		zap.Duration("ttl", time.Until(st.ExpiresAt)),
		zap.String("component_uid", claims.ComponentUID),
		zap.String("environment_uid", claims.EnvironmentUID),
	)

	return nil
}

// refreshLoop sleeps until the token nears expiry, then acquires a new one.
// On failure, retries with exponential backoff (1s, 2s, 4s... up to 60s).
func (tm *TokenManager) refreshLoop(ctx context.Context) {
	for {
		st := tm.state()
		if st == nil {
			tm.logger.Error("refresh loop: no token state — exiting")
			return
		}

		// Wake up at (expires_at - buffer).
		wakeAt := st.ExpiresAt.Add(-tm.cfg.TokenRefreshBuffer)
		sleepDur := time.Until(wakeAt)
		if sleepDur < 0 {
			// Already past the refresh window — refresh immediately.
			sleepDur = 0
		}

		tm.logger.Debug("refresh scheduled",
			zap.Time("wake_at", wakeAt),
			zap.Duration("sleep", sleepDur),
		)

		select {
		case <-ctx.Done():
			tm.logger.Info("refresh goroutine stopped")
			return
		case <-time.After(sleepDur):
		}

		// Exponential backoff retry loop.
		attempt := 0
		for {
			err := tm.acquire()
			if err == nil {
				break // success — outer loop will recalculate next wake time
			}

			attempt++
			backoff := time.Duration(math.Min(
				float64(time.Second)*math.Pow(2, float64(attempt-1)),
				float64(60*time.Second),
			))

			// Check if the token expired during retries.
			if tm.IsExpired() {
				tm.logger.Error("token EXPIRED during refresh retry — agent requests will fail",
					zap.Int("attempt", attempt),
					zap.Error(err),
				)
			} else {
				tm.logger.Warn("token refresh failed, retrying",
					zap.Int("attempt", attempt),
					zap.Duration("backoff", backoff),
					zap.Error(err),
				)
			}

			select {
			case <-ctx.Done():
				tm.logger.Info("refresh goroutine stopped during retry")
				return
			case <-time.After(backoff):
			}
		}
	}
}

// parseAgentClaims decodes the JWT without signature verification.
// We trust the token we just received from the AMP API.
func parseAgentClaims(rawToken string) (*agentTokenClaims, error) {
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	token, _, err := parser.ParseUnverified(rawToken, &agentTokenClaims{})
	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(*agentTokenClaims)
	if !ok {
		return nil, fmt.Errorf("unexpected claims type: %T", token.Claims)
	}
	return claims, nil
}

// ParseScopesFromJWT extracts the "scope" claim from any JWT as a string slice.
// WSO2 IS uses a space-delimited "scope" claim. This function handles:
//   - space-delimited string:  "read:docs write:orders"  → ["read:docs", "write:orders"]
//   - empty/missing scope:     → nil
//
// The JWT is parsed without signature verification (decode-only, as per BRD
// decision — gateway has already validated the signature).
func ParseScopesFromJWT(rawToken string) ([]string, error) {
	// Use a generic map claims parser to handle any JWT structure.
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	token, _, err := parser.ParseUnverified(rawToken, jwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("parse JWT: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("unexpected claims type: %T", token.Claims)
	}

	scopeRaw, exists := claims["scope"]
	if !exists {
		return nil, nil
	}

	switch v := scopeRaw.(type) {
	case string:
		scopes := strings.Fields(v)
		if len(scopes) == 0 {
			return nil, nil
		}
		return scopes, nil
	default:
		return nil, fmt.Errorf("unsupported scope claim type: %T", scopeRaw)
	}
}

// IntersectScopes returns the intersection of two scope slices.
// If either input is nil/empty, the other is returned as-is (fail-open for
// the case where no employee token is present).
func IntersectScopes(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	set := make(map[string]struct{}, len(a))
	for _, s := range a {
		set[s] = struct{}{}
	}
	var result []string
	for _, s := range b {
		if _, ok := set[s]; ok {
			result = append(result, s)
		}
	}
	return result
}
