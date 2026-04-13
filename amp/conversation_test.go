package amp

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestConversationManagerGetOrCreate(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cm := NewConversationManager(30*time.Minute, logger)
	defer cm.Shutdown()

	// Create new conversation with empty ID.
	conv, id := cm.GetOrCreate("")
	if id == "" {
		t.Error("expected generated UUID, got empty string")
	}
	if conv == nil {
		t.Fatal("expected non-nil conversation")
	}
	if conv.ID != id {
		t.Errorf("conv.ID = %q, want %q", conv.ID, id)
	}

	// Retrieve existing conversation.
	conv2, id2 := cm.GetOrCreate(id)
	if id2 != id {
		t.Errorf("expected same ID %q, got %q", id, id2)
	}
	if conv2 != conv {
		t.Error("expected same conversation pointer")
	}

	// Create with explicit ID.
	conv3, id3 := cm.GetOrCreate("custom-id-123")
	if id3 != "custom-id-123" {
		t.Errorf("expected custom-id-123, got %q", id3)
	}
	if conv3 == nil {
		t.Fatal("expected non-nil conversation")
	}
}

func TestConversationAppendExchange(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cm := NewConversationManager(30*time.Minute, logger)
	defer cm.Shutdown()

	conv, _ := cm.GetOrCreate("")

	conv.AppendExchange("Hello", "Hi there!")
	conv.AppendExchange("How are you?", "I'm doing well.")

	msgs := conv.GetMessages()
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}

	expected := []struct {
		role    string
		content string
	}{
		{"user", "Hello"},
		{"assistant", "Hi there!"},
		{"user", "How are you?"},
		{"assistant", "I'm doing well."},
	}

	for i, exp := range expected {
		if msgs[i].Role != exp.role {
			t.Errorf("msgs[%d].Role = %q, want %q", i, msgs[i].Role, exp.role)
		}
		if msgs[i].Content != exp.content {
			t.Errorf("msgs[%d].Content = %q, want %q", i, msgs[i].Content, exp.content)
		}
	}
}

func TestConversationGetMessagesCopy(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cm := NewConversationManager(30*time.Minute, logger)
	defer cm.Shutdown()

	conv, _ := cm.GetOrCreate("")
	conv.AppendExchange("Hello", "Hi")

	// Get messages returns a copy — mutating it shouldn't affect the conversation.
	msgs := conv.GetMessages()
	msgs[0].Content = "MUTATED"

	original := conv.GetMessages()
	if original[0].Content == "MUTATED" {
		t.Error("GetMessages should return a copy, not a reference")
	}
}

func TestConversationEviction(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	// Use a very short TTL.
	cm := NewConversationManager(50*time.Millisecond, logger)
	defer cm.Shutdown()

	conv, id := cm.GetOrCreate("")
	if conv == nil {
		t.Fatal("expected non-nil conversation")
	}

	// Wait for TTL + margin.
	time.Sleep(100 * time.Millisecond)

	// Force eviction (the loop runs every 60s, so call it directly).
	cm.evict()

	// Should be gone.
	if got := cm.Get(id); got != nil {
		t.Error("expected conversation to be evicted")
	}
}

func TestConversationEvictionKeepsActive(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cm := NewConversationManager(200*time.Millisecond, logger)
	defer cm.Shutdown()

	_, id := cm.GetOrCreate("")

	// Touch it within TTL.
	time.Sleep(100 * time.Millisecond)
	cm.GetOrCreate(id) // updates LastUsed

	time.Sleep(50 * time.Millisecond)
	cm.evict()

	// Should still be present — LastUsed was updated.
	if got := cm.Get(id); got == nil {
		t.Error("expected conversation to survive eviction (was recently touched)")
	}
}

func TestConversationConcurrentAccess(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cm := NewConversationManager(30*time.Minute, logger)
	defer cm.Shutdown()

	conv, _ := cm.GetOrCreate("concurrent-test")

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			conv.AppendExchange("msg", "reply")
			_ = conv.GetMessages()
		}(i)
	}
	wg.Wait()

	msgs := conv.GetMessages()
	if len(msgs) != 200 { // 100 exchanges × 2 messages each
		t.Errorf("expected 200 messages, got %d", len(msgs))
	}
}

func TestConversationDelete(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	cm := NewConversationManager(30*time.Minute, logger)
	defer cm.Shutdown()

	_, id := cm.GetOrCreate("")
	cm.Delete(id)

	if got := cm.Get(id); got != nil {
		t.Error("expected nil after Delete")
	}
}
