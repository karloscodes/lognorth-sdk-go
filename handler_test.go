package lognorth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestLog(t *testing.T) {
	var received []map[string]any
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var data map[string]any
		json.Unmarshal(body, &data)
		mu.Lock()
		received = append(received, data)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer server.Close()

	Config(server.URL, "test-key")

	Log("User signed up", map[string]any{"user_id": 123})
	Flush()

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 1 {
		t.Fatalf("expected 1 request, got %d", len(received))
	}

	events := received[0]["events"].([]any)
	event := events[0].(map[string]any)
	if event["message"] != "User signed up" {
		t.Errorf("expected message 'User signed up', got %v", event["message"])
	}

	ctx := event["context"].(map[string]any)
	if ctx["user_id"] != float64(123) {
		t.Errorf("expected user_id 123, got %v", ctx["user_id"])
	}
}

func TestError(t *testing.T) {
	var received []map[string]any
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var data map[string]any
		json.Unmarshal(body, &data)
		mu.Lock()
		received = append(received, data)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer server.Close()

	Config(server.URL, "test-key")

	Error("Checkout failed", fmt.Errorf("connection refused"), map[string]any{"order_id": 42})

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(received) < 1 {
		t.Fatalf("expected at least 1 request, got %d", len(received))
	}

	events := received[0]["events"].([]any)
	event := events[0].(map[string]any)
	ctx := event["context"].(map[string]any)
	if ctx["error_class"] == nil || ctx["error_class"] == "" {
		t.Errorf("expected error_class in context, got %v", ctx["error_class"])
	}
	if ctx["error_file"] == nil || ctx["error_file"] == "" {
		t.Errorf("expected error_file in context, got %v", ctx["error_file"])
	}
	if ctx["error_line"] == nil || ctx["error_line"] == float64(0) {
		t.Errorf("expected error_line in context, got %v", ctx["error_line"])
	}
	if ctx["stack_trace"] == nil || ctx["stack_trace"] == "" {
		t.Errorf("expected stack_trace in context, got %v", ctx["stack_trace"])
	}
}

func TestAuthHeader(t *testing.T) {
	var authHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer server.Close()

	Config(server.URL, "my-secret-key")

	Log("Test", nil)
	Flush()

	time.Sleep(50 * time.Millisecond)

	if authHeader != "Bearer my-secret-key" {
		t.Errorf("expected auth header 'Bearer my-secret-key', got '%s'", authHeader)
	}
}

func TestMiddlewareGeneratesTraceID(t *testing.T) {
	var received []map[string]any
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var data map[string]any
		json.Unmarshal(body, &data)
		mu.Lock()
		received = append(received, data)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer server.Close()

	Config(server.URL, "test-key")

	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify trace_id is in request context
		traceID := traceIDFromContext(r.Context())
		if traceID == "" {
			t.Error("expected trace_id in request context")
		}
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Check response header
	if rr.Header().Get("X-Trace-ID") == "" {
		t.Error("expected X-Trace-ID response header")
	}

	Flush()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(received) < 1 {
		t.Fatalf("expected at least 1 request, got %d", len(received))
	}

	events := received[0]["events"].([]any)
	event := events[0].(map[string]any)

	if event["trace_id"] == nil || event["trace_id"] == "" {
		t.Error("expected trace_id on event")
	}
	if event["duration_ms"] == nil {
		t.Error("expected duration_ms as top-level field")
	}
	// duration_ms should NOT be in context
	ctx := event["context"].(map[string]any)
	if ctx["duration_ms"] != nil {
		t.Error("duration_ms should not be in context")
	}
}

func TestMiddlewareUsesIncomingTraceID(t *testing.T) {
	var received []map[string]any
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var data map[string]any
		json.Unmarshal(body, &data)
		mu.Lock()
		received = append(received, data)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer server.Close()

	Config(server.URL, "test-key")

	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Trace-ID", "incoming-trace")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Header().Get("X-Trace-ID") != "incoming-trace" {
		t.Errorf("expected X-Trace-ID 'incoming-trace', got '%s'", rr.Header().Get("X-Trace-ID"))
	}

	Flush()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	events := received[0]["events"].([]any)
	event := events[0].(map[string]any)
	if event["trace_id"] != "incoming-trace" {
		t.Errorf("expected trace_id 'incoming-trace', got %v", event["trace_id"])
	}
}
