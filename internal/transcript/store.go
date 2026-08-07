// Package transcript holds the gateway's per-sender rolling window of recent
// turns (ADR-0006 §1). The window is replayed in the messages array of each
// chat/completions request so Hermes has short-term conversational continuity
// for deictic confirmation ("draft the email to Bob… yeah, send it" resolves
// "it" against the prior assistant draft).
//
// Contract per ADR-0006 §1:
//   - count-only, no time-trim (a stale window is superseded by Hermes's
//     long-term memory; time-trim is a knob with no measured benefit)
//   - pruned to user+assistant turns (canned-fallback and system turns
//     are excluded)
//   - lost on restart (in-memory, single replica under ADR-0004)
//   - default K ≈ 2–3 retained exchanges (passed as windowTurns)
package transcript

import (
	"sync"
)

// Turn is one replayed conversation turn. Role is "user" or "assistant".
type Turn struct {
	Role    string
	Content string
}

// Store is the per-sender rolling window. It is safe for concurrent use by
// the SMS handler and the metrics/observability paths.
type Store struct {
	mu          sync.Mutex
	bySender    map[string][]Turn
	windowTurns int
}

// New returns a Store that retains at most windowTurns exchanges per sender
// (an exchange is one user + one assistant turn). A turn that does not have
// a counterpart is still retained; the trim keeps the last 2*windowTurns
// entries (each entry being a single Turn).
func New(windowTurns int) *Store {
	if windowTurns < 1 {
		windowTurns = 1
	}
	return &Store{
		bySender:    make(map[string][]Turn),
		windowTurns: windowTurns,
	}
}

// Append records a turn for sender. Only "user" and "assistant" turns are
// stored — a turn with any other role is dropped (pruned-content contract).
// The trim keeps the last 2*windowTurns turns (K user + K assistant pairs).
func (s *Store) Append(sender, role, content string) {
	if role != "user" && role != "assistant" {
		return
	}
	if content == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	max := s.windowTurns * 2
	turns := append(s.bySender[sender], Turn{Role: role, Content: content})
	if len(turns) > max {
		turns = turns[len(turns)-max:]
	}
	s.bySender[sender] = turns
}

// Window returns a copy of the sender's pruned turn window, oldest first.
// The new user message is NOT included — the caller appends it.
func (s *Store) Window(sender string) []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	turns := s.bySender[sender]
	if len(turns) == 0 {
		return nil
	}
	out := make([]Turn, len(turns))
	copy(out, turns)
	return out
}

// Clear drops the window for sender. Not currently called by any production
// path — kept for completeness and tests. A restart is the only "clear" the
// SMS path relies on (ADR-0004 in-memory posture).
func (s *Store) Clear(sender string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bySender, sender)
}
