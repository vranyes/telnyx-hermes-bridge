package telnyx

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// reqRecorder captures a single outbound request so a test can assert on the
// method, path, auth header, and decoded JSON body.
type reqRecorder struct {
	method string
	path   string
	auth   string
	body   map[string]any
}

// captureServer is an httptest server that records every request it receives.
type captureServer struct {
	t      *testing.T
	status int
	http   *httptest.Server
	mu     sync.Mutex
	reqs   []reqRecorder
}

func newCaptureServer(t *testing.T, status int) *captureServer {
	t.Helper()
	s := &captureServer{t: t, status: status}
	s.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := reqRecorder{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") && len(b) > 0 {
			if err := json.Unmarshal(b, &rec.body); err != nil {
				t.Fatalf("decode body %q: %v", b, err)
			}
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, rec)
		s.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(s.http.Close)
	return s
}

func (s *captureServer) first() reqRecorder {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		s.t.Fatal("no Telnyx request recorded")
	}
	return s.reqs[0]
}

func newTestClient(t *testing.T, baseURL string, status int) (*Client, *captureServer) {
	t.Helper()
	srv := newCaptureServer(t, status)
	base := baseURL
	if base == "" {
		base = srv.http.URL
	}
	return NewClient("test-key", base, "+15559876543", srv.http.Client(), discardLogger()), srv
}

func TestClient_SendSMS(t *testing.T) {
	c, srv := newTestClient(t, "", http.StatusOK)
	if err := c.SendSMS(context.Background(), "+15551234567", "hello"); err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	r := srv.first()
	if r.method != http.MethodPost || r.path != "/messages" {
		t.Fatalf("got %s %s, want POST /messages", r.method, r.path)
	}
	if r.auth != "Bearer test-key" {
		t.Fatalf("auth = %q", r.auth)
	}
	if r.body["from"] != "+15559876543" || r.body["to"] != "+15551234567" || r.body["text"] != "hello" {
		t.Fatalf("bad body: %v", r.body)
	}
}

func TestClient_SendSMS_Non2xxErrors(t *testing.T) {
	c, _ := newTestClient(t, "", http.StatusBadGateway)
	if err := c.SendSMS(context.Background(), "+15551234567", "hi"); err == nil {
		t.Fatal("want error on non-2xx")
	}
}

func TestClient_BaseURLTrailingSlashTrimmed(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK)
	c := NewClient("k", srv.http.URL+"/", "+15559876543", srv.http.Client(), discardLogger())
	if err := c.SendSMS(context.Background(), "+15551234567", "x"); err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	if r := srv.first(); r.path != "/messages" {
		t.Fatalf("path = %q, want /messages (double slash must be trimmed)", r.path)
	}
}
