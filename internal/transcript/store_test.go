package transcript

import (
	"testing"
)

func TestStore_EmptySender(t *testing.T) {
	s := New(3)
	if got := s.Window("nobody"); got != nil {
		t.Fatalf("Window = %v, want nil", got)
	}
}

func TestStore_AppendsAndReturnsInOrder(t *testing.T) {
	s := New(3)
	s.Append("a", "user", "hello")
	s.Append("a", "assistant", "hi back")
	got := s.Window("a")
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Role != "user" || got[0].Content != "hello" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].Role != "assistant" || got[1].Content != "hi back" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestStore_TrimsToKExchanges(t *testing.T) {
	s := New(2) // 2 exchanges = up to 4 turns
	for i := 0; i < 6; i++ {
		s.Append("a", "user", "u")
		s.Append("a", "assistant", "a")
	}
	got := s.Window("a")
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4 (2 K-pairs)", len(got))
	}
}

func TestStore_PerSenderIsolation(t *testing.T) {
	s := New(3)
	s.Append("alice", "user", "alice msg")
	s.Append("bob", "user", "bob msg")
	if got := s.Window("alice"); len(got) != 1 || got[0].Content != "alice msg" {
		t.Fatalf("alice window = %v", got)
	}
	if got := s.Window("bob"); len(got) != 1 || got[0].Content != "bob msg" {
		t.Fatalf("bob window = %v", got)
	}
}

func TestStore_OnlyUserAndAssistantStored(t *testing.T) {
	// Canned-fallback / system turns must NOT enter the window (ADR-0006 §1
	// "pruned to user+assistant turns").
	s := New(3)
	s.Append("a", "system", "system msg")
	s.Append("a", "assistant", "real reply")
	s.Append("a", "tool", "tool call")
	if got := s.Window("a"); len(got) != 1 || got[0].Role != "assistant" {
		t.Fatalf("non-user/assistant turns entered the window: %v", got)
	}
}

func TestStore_EmptyContentDropped(t *testing.T) {
	s := New(3)
	// An empty assistant reply (the soft-miss case per ADR-0006 §1) must not
	// enter the window — it would replay as a turn that did nothing.
	s.Append("a", "user", "hello")
	s.Append("a", "assistant", "")
	got := s.Window("a")
	if len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("empty assistant reply should be dropped: %v", got)
	}
}

func TestStore_LostOnRestart(t *testing.T) {
	// A new Store has no memory of prior stores — the in-memory posture.
	s1 := New(3)
	s1.Append("a", "user", "first")
	s2 := New(3)
	if got := s2.Window("a"); got != nil {
		t.Fatalf("fresh store returned non-nil: %v", got)
	}
}

func TestStore_WindowReturnsCopy(t *testing.T) {
	// Mutating the returned slice must not corrupt future Windows — the SMS
	// handler builds the messages array from the slice and the underlying
	// store must stay detached.
	s := New(3)
	s.Append("a", "user", "first")
	got := s.Window("a")
	got[0] = Turn{Role: "assistant", Content: "tampered"}
	again := s.Window("a")
	if again[0].Role != "user" || again[0].Content != "first" {
		t.Fatalf("Window returned a shared slice; got %v", again)
	}
}
