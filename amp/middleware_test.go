package amp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

func TestConversationMiddleware_GeneratesID(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cm := NewConversationManager(30*time.Minute, logger)
	defer cm.Shutdown()

	var capturedID string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = ConversationIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	middleware := ConversationMiddleware(cm, logger)(handler)

	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	rr := httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if capturedID == "" {
		t.Error("expected generated conversation ID in context")
	}

	// Should be echoed in response header.
	respID := rr.Header().Get("X-Conversation-ID")
	if respID != capturedID {
		t.Errorf("response X-Conversation-ID = %q, want %q", respID, capturedID)
	}
}

func TestConversationMiddleware_EchoesExistingID(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cm := NewConversationManager(30*time.Minute, logger)
	defer cm.Shutdown()

	// Pre-create a conversation.
	_, existingID := cm.GetOrCreate("test-conv-123")

	var capturedID string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = ConversationIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	middleware := ConversationMiddleware(cm, logger)(handler)

	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header.Set("X-Conversation-ID", existingID)
	rr := httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if capturedID != existingID {
		t.Errorf("context conv ID = %q, want %q", capturedID, existingID)
	}

	if rr.Header().Get("X-Conversation-ID") != existingID {
		t.Errorf("response header = %q, want %q", rr.Header().Get("X-Conversation-ID"), existingID)
	}
}

func TestLeastPrivilegeMiddleware_NoUserToken(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	agentScopes := []string{"read:docs", "read:orders"}

	var capturedScope []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedScope = EffectiveScopeFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	middleware := LeastPrivilegeMiddleware(agentScopes, logger)(handler)

	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	// No X-User-Token header.
	rr := httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Should get full agent scope when no employee token.
	if !slicesEqual(capturedScope, agentScopes) {
		t.Errorf("effective_scope = %v, want %v", capturedScope, agentScopes)
	}
}

func TestLeastPrivilegeMiddleware_PartialOverlap(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	agentScopes := []string{"read:docs", "read:orders"}

	// Create employee JWT with partial overlap.
	employeeJWT := signTestMapJWT(t, jwt.MapClaims{
		"sub":   "employee-1",
		"scope": "read:docs write:orders admin:settings",
	})

	var capturedScope []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedScope = EffectiveScopeFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	middleware := LeastPrivilegeMiddleware(agentScopes, logger)(handler)

	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header.Set("X-User-Token", employeeJWT)
	rr := httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Intersection: employee has read:docs (overlap), write:orders (no match for "read:orders")
	// Agent has read:docs, read:orders
	// Intersection = [read:docs]
	want := []string{"read:docs"}
	if !slicesEqual(capturedScope, want) {
		t.Errorf("effective_scope = %v, want %v", capturedScope, want)
	}
}

func TestLeastPrivilegeMiddleware_EmptyIntersection_403(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	agentScopes := []string{"admin:settings"}

	// Employee has no admin scopes.
	employeeJWT := signTestMapJWT(t, jwt.MapClaims{
		"sub":   "employee-1",
		"scope": "read:docs read:orders",
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called on 403")
	})

	middleware := LeastPrivilegeMiddleware(agentScopes, logger)(handler)

	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header.Set("X-User-Token", employeeJWT)
	rr := httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}

	var body map[string]string
	json.NewDecoder(rr.Body).Decode(&body)
	if body["error"] != "no effective scope" {
		t.Errorf("error = %q, want %q", body["error"], "no effective scope")
	}
}

func TestLeastPrivilegeMiddleware_InvalidToken_401(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	agentScopes := []string{"read:docs"}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called on 401")
	})

	middleware := LeastPrivilegeMiddleware(agentScopes, logger)(handler)

	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header.Set("X-User-Token", "not-a-valid-jwt")
	rr := httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestLeastPrivilegeMiddleware_UserTokenInjected(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	agentScopes := []string{"read:docs"}

	employeeJWT := signTestMapJWT(t, jwt.MapClaims{
		"sub":   "employee-1",
		"scope": "read:docs",
	})

	var capturedToken string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedToken = UserTokenFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	middleware := LeastPrivilegeMiddleware(agentScopes, logger)(handler)

	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header.Set("X-User-Token", employeeJWT)
	rr := httptest.NewRecorder()

	middleware.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if capturedToken != employeeJWT {
		t.Error("expected employee JWT in context")
	}
}

func TestMiddlewareChain(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cm := NewConversationManager(30*time.Minute, logger)
	defer cm.Shutdown()

	agentScopes := []string{"read:docs", "read:orders"}

	employeeJWT := signTestMapJWT(t, jwt.MapClaims{
		"sub":   "employee-1",
		"scope": "read:docs write:docs",
	})

	var capturedConvID string
	var capturedScope []string

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedConvID = ConversationIDFromContext(r.Context())
		capturedScope = EffectiveScopeFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	// Chain: ConversationMiddleware → LeastPrivilegeMiddleware → handler
	chain := ConversationMiddleware(cm, logger)(
		LeastPrivilegeMiddleware(agentScopes, logger)(handler),
	)

	req := httptest.NewRequest(http.MethodPost, "/chat", nil)
	req.Header.Set("X-User-Token", employeeJWT)
	rr := httptest.NewRecorder()

	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	if capturedConvID == "" {
		t.Error("expected conversation ID in context")
	}

	// Intersection: employee [read:docs, write:docs] ∩ agent [read:docs, read:orders] = [read:docs]
	want := []string{"read:docs"}
	if !slicesEqual(capturedScope, want) {
		t.Errorf("effective_scope = %v, want %v", capturedScope, want)
	}

	// Verify X-Conversation-ID is in response.
	if rr.Header().Get("X-Conversation-ID") == "" {
		t.Error("expected X-Conversation-ID in response headers")
	}
}

func TestContextHelpers_EmptyContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	scope := EffectiveScopeFromContext(req.Context())
	if scope != nil {
		t.Errorf("EffectiveScopeFromContext on empty context = %v, want nil", scope)
	}

	convID := ConversationIDFromContext(req.Context())
	if convID != "" {
		t.Errorf("ConversationIDFromContext on empty context = %q, want empty", convID)
	}

	token := UserTokenFromContext(req.Context())
	if token != "" {
		t.Errorf("UserTokenFromContext on empty context = %q, want empty", token)
	}
}
