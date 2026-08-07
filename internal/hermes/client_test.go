package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vranyes/telnyx-hermes-bridge/internal/metrics"
)

func newTestClient(t *testing.T, srv *httptest.Server, budget time.Duration) *Client {
	t.Helper()
	m := &metrics.Metrics{}
	c := NewClient(srv.URL, "/v1/chat/completions", "shared-secret", budget, srv.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	return c
}

func chatBody(reply string) []byte {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"role": "assistant", "content": reply}},
		},
	})
	return b
}

func TestTurn_SucceedsFirstTry(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if got := r.Header.Get("Idempotency-Key"); got != "evt-1" {
			t.Errorf("Idempotency-Key = %q, want evt-1", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer shared-secret" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chatBody("hello back"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, time.Second)
	res, err := c.Turn(context.Background(), "evt-1", "+1555", Request{Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Reply != "hello back" {
		t.Fatalf("Reply = %q, want hello back", res.Reply)
	}
	if calls != 1 {
		t.Fatalf("want 1 call, got %d", calls)
	}
}

func TestTurn_EmptyReplyIsSoftMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No content -> soft-miss. Do NOT trigger the fallback.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chatBody(""))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, time.Second)
	_, err := c.Turn(context.Background(), "evt-2", "+1555", Request{Messages: nil})
	if !errors.Is(err, ErrEmptyReply) {
		t.Fatalf("err = %v, want ErrEmptyReply", err)
	}
}

func TestTurn_NoChoicesIsSoftMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, time.Second)
	_, err := c.Turn(context.Background(), "evt-3", "+1555", Request{})
	if !errors.Is(err, ErrEmptyReply) {
		t.Fatalf("err = %v, want ErrEmptyReply", err)
	}
}

func TestTurn_Retries5xxThenSucceeds(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(chatBody("ok now"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 2*time.Second)
	res, err := c.Turn(context.Background(), "evt-4", "+1555", Request{})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Reply != "ok now" {
		t.Fatalf("Reply = %q", res.Reply)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

func TestTurn_429RetriesAndHonorsRetryAfter(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(chatBody("after 429"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 2*time.Second)
	res, err := c.Turn(context.Background(), "evt-5", "+1555", Request{})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Reply != "after 429" {
		t.Fatalf("Reply = %q", res.Reply)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

func TestTurn_4xxIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, time.Second)
	_, err := c.Turn(context.Background(), "evt-6", "+1555", Request{})
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
}

func TestTurn_ExhaustedReturnsErrExhausted(t *testing.T) {
	// A 503 every attempt exhausts the budget; the client returns
	// ErrExhausted. The canned SMS fallback is NOT sent by the client
	// — the SMS handler (runTurn) owns that single send path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 300*time.Millisecond)
	_, err := c.Turn(context.Background(), "evt-7", "+1555", Request{})
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
}

func TestTurn_BadJSONRetried(t *testing.T) {
	// A 2xx with garbage body is treated as retryable; the budget then runs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, 300*time.Millisecond)
	_, err := c.Turn(context.Background(), "evt-8", "+1555", Request{})
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted (bad JSON retries until budget)", err)
	}
}

func TestSchedule_UsesRealBackoff(t *testing.T) {
	s := schedule(60 * time.Second)
	if len(s) != 6 {
		t.Fatalf("want 6 steps, got %d", len(s))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 30 * time.Second}
	for i, d := range s {
		lo := want[i] * 4 / 5
		hi := want[i] * 6 / 5
		if d < lo || d > hi {
			t.Fatalf("step %d = %v, want within [%v, %v]", i, d, lo, hi)
		}
	}
}

func TestSchedule_ScalesWithBudget(t *testing.T) {
	s := schedule(150 * time.Millisecond)
	if len(s) != 6 {
		t.Fatalf("want 6 steps, got %d", len(s))
	}
	total := time.Duration(0)
	for _, d := range s {
		if d < 0 {
			t.Fatalf("negative delay %v", d)
		}
		total += d
	}
	if total > 190*time.Millisecond {
		t.Fatalf("scaled schedule total %v exceeds the 150ms budget", total)
	}
}
