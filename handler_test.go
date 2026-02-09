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
	if event["error_type"] != "Error" {
		t.Errorf("expected error_type 'Error', got %v", event["error_type"])
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
