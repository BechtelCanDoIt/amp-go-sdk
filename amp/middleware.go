package amp

import (
	"context"
	"encoding/json"
	"net/http"

	"go.uber.org/zap"
)

// Context keys for values injected by middleware.
type contextKey string

const (
	ContextKeyEffectiveScope contextKey = "amp.effective_scope"
	ContextKeyConversationID contextKey = "amp.conversation_id"
	ContextKeyUserToken      contextKey = "amp.user_token"
)

// EffectiveScopeFromContext extracts the intersected scope slice injected
// by LeastPrivilegeMiddleware.
func EffectiveScopeFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(ContextKeyEffectiveScope).([]string)
	return v
}

// ConversationIDFromContext extracts the conversation ID injected by
// ConversationMiddleware.
func ConversationIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ContextKeyConversationID).(string)
	return v
}

// UserTokenFromContext extracts the raw employee JWT injected by
// LeastPrivilegeMiddleware.
func UserTokenFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ContextKeyUserToken).(string)
	return v
}

// middlewareError writes a JSON error response and logs it.
func middlewareError(w http.ResponseWriter, code int, msg string, logger *zap.Logger) {
	logger.Warn("middleware rejection", zap.Int("status", code), zap.String("error", msg))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// LeastPrivilegeMiddleware enforces the intersection model described in
// BRD §11. It reads X-User-Token, computes the intersection with the
// agent's scopes, and injects the effective_scope into the request context.
//
// Behavior:
//   - X-User-Token absent:  inject agent's full scope (no employee constraint)
//   - X-User-Token present but invalid JWT: 401
//   - Intersection empty: 403 {"error": "no effective scope"}
//   - Intersection non-empty: inject effective_scope, log decision, call next
//
// Note: The agent JWT (AgentTokenClaims) does NOT carry scopes today.
// For forward-compatibility, agent scopes are passed via the agentScopes
// parameter. In production, these come from agent registration metadata.
func LeastPrivilegeMiddleware(
	agentScopes []string,
	logger *zap.Logger,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			convID := ConversationIDFromContext(r.Context())

			userToken := r.Header.Get("X-User-Token")
			if userToken == "" {
				// No employee token — agent operates with its full scope.
				ctx := context.WithValue(r.Context(), ContextKeyEffectiveScope, agentScopes)
				logger.Debug("least-priv: no employee token, using full agent scope",
					zap.String("conversation_id", convID),
					zap.Strings("effective_scope", agentScopes),
				)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Decode employee JWT (trust gateway validated signature).
			employeeScopes, err := ParseScopesFromJWT(userToken)
			if err != nil {
				middlewareError(w, http.StatusUnauthorized,
					"invalid X-User-Token: "+err.Error(), logger)
				return
			}

			effective := IntersectScopes(employeeScopes, agentScopes)

			// Log the decision (structured JSON as per BRD §11.2).
			logger.Info("least-priv decision",
				zap.String("conversation_id", convID),
				zap.Strings("employee_scopes", employeeScopes),
				zap.Strings("agent_scopes", agentScopes),
				zap.Strings("effective_scope", effective),
				zap.String("decision", decisionLabel(effective)),
			)

			if len(effective) == 0 {
				middlewareError(w, http.StatusForbidden, "no effective scope", logger)
				return
			}

			ctx := r.Context()
			ctx = context.WithValue(ctx, ContextKeyEffectiveScope, effective)
			ctx = context.WithValue(ctx, ContextKeyUserToken, userToken)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ConversationMiddleware reads X-Conversation-ID, generates a UUID v4 if
// absent, injects the ID into the request context, and sets the header on
// the response for the client to echo on subsequent turns.
func ConversationMiddleware(
	cm *ConversationManager,
	logger *zap.Logger,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			incomingID := r.Header.Get("X-Conversation-ID")
			_, convID := cm.GetOrCreate(incomingID)

			// Set on response so the client can echo it back.
			w.Header().Set("X-Conversation-ID", convID)

			ctx := context.WithValue(r.Context(), ContextKeyConversationID, convID)
			logger.Debug("conversation middleware",
				zap.String("incoming_id", incomingID),
				zap.String("conversation_id", convID),
			)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func decisionLabel(effective []string) string {
	if len(effective) == 0 {
		return "denied"
	}
	return "allowed"
}
