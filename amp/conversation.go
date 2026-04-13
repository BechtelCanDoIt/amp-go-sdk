package amp

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Conversation holds the message history and metadata for a single conversation.
type Conversation struct {
	ID        string
	Messages  []Message
	CreatedAt time.Time
	LastUsed  time.Time
	mu        sync.Mutex
}

// AppendExchange adds a user message and assistant reply to the conversation
// history, updating the last-used timestamp.
func (c *Conversation) AppendExchange(userMsg, assistantReply string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Messages = append(c.Messages,
		Message{Role: "user", Content: userMsg},
		Message{Role: "assistant", Content: assistantReply},
	)
	c.LastUsed = time.Now()
}

// GetMessages returns a copy of the message history (safe for concurrent use).
func (c *Conversation) GetMessages() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastUsed = time.Now()
	msgs := make([]Message, len(c.Messages))
	copy(msgs, c.Messages)
	return msgs
}

// ConversationManager provides a thread-safe, TTL-evicting in-memory store
// for multi-turn conversations, keyed by conversation ID (UUID v4).
type ConversationManager struct {
	store  sync.Map // map[string]*Conversation
	ttl    time.Duration
	logger *zap.Logger
	cancel context.CancelFunc
}

// NewConversationManager creates a manager with the given TTL and starts the
// background eviction goroutine.
func NewConversationManager(ttl time.Duration, logger *zap.Logger) *ConversationManager {
	ctx, cancel := context.WithCancel(context.Background())
	cm := &ConversationManager{
		ttl:    ttl,
		logger: logger.Named("conversation"),
		cancel: cancel,
	}
	go cm.evictionLoop(ctx)
	return cm
}

// GetOrCreate retrieves an existing conversation by ID, or creates a new one
// if the ID is empty or not found. Returns the conversation and its ID.
func (cm *ConversationManager) GetOrCreate(id string) (*Conversation, string) {
	if id != "" {
		if v, ok := cm.store.Load(id); ok {
			conv := v.(*Conversation)
			conv.mu.Lock()
			conv.LastUsed = time.Now()
			conv.mu.Unlock()
			return conv, id
		}
	}

	// New conversation.
	if id == "" {
		id = uuid.New().String()
	}

	now := time.Now()
	conv := &Conversation{
		ID:        id,
		Messages:  make([]Message, 0, 20),
		CreatedAt: now,
		LastUsed:  now,
	}

	// LoadOrStore handles the race where two requests create the same ID.
	actual, loaded := cm.store.LoadOrStore(id, conv)
	if loaded {
		return actual.(*Conversation), id
	}

	cm.logger.Debug("conversation created", zap.String("id", id))
	return conv, id
}

// Get retrieves a conversation by ID. Returns nil if not found.
func (cm *ConversationManager) Get(id string) *Conversation {
	if v, ok := cm.store.Load(id); ok {
		return v.(*Conversation)
	}
	return nil
}

// Delete removes a conversation by ID.
func (cm *ConversationManager) Delete(id string) {
	cm.store.Delete(id)
}

// Shutdown stops the eviction goroutine.
func (cm *ConversationManager) Shutdown() {
	if cm.cancel != nil {
		cm.cancel()
	}
}

// evictionLoop runs every 60 seconds and removes conversations that have been
// idle longer than the configured TTL.
func (cm *ConversationManager) evictionLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cm.evict()
		}
	}
}

func (cm *ConversationManager) evict() {
	cutoff := time.Now().Add(-cm.ttl)
	evicted := 0

	cm.store.Range(func(key, value any) bool {
		conv := value.(*Conversation)
		conv.mu.Lock()
		lastUsed := conv.LastUsed
		conv.mu.Unlock()

		if lastUsed.Before(cutoff) {
			cm.store.Delete(key)
			evicted++
		}
		return true
	})

	if evicted > 0 {
		cm.logger.Debug("evicted idle conversations", zap.Int("count", evicted))
	}
}
