package lognorth

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHandler_BasicLogging(t *testing.T) {
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

	Config("test-key", server.URL)
	log := slog.New(NewHandler())

	log.Info("User signed up", "user_id", 123)
	Flush()

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 1 {
		t.Fatalf("expected 1 request, got %d", len(received))
	}

	events := received[0]["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	event := events[0].(map[string]any)
	if event["message"] != "User signed up" {
		t.Errorf("expected message 'User signed up', got %v", event["message"])
	}

	ctx := event["context"].(map[string]any)
	if ctx["user_id"] != float64(123) {
		t.Errorf("expected user_id 123, got %v", ctx["user_id"])
	}
}

func TestHandler_ErrorsSentImmediately(t *testing.T) {
	var requestCount int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer server.Close()

	Config("test-key", server.URL)
	log := slog.New(NewHandler())

	log.Error("Something failed", "error", "connection refused")

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if requestCount < 1 {
		t.Errorf("expected error to be sent immediately, got %d requests", requestCount)
	}
}

func TestHandler_AuthHeader(t *testing.T) {
	var authHeader string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer server.Close()

	Config("my-secret-key", server.URL)
	log := slog.New(NewHandler())

	log.Info("Test")
	Flush()

	time.Sleep(50 * time.Millisecond)

	if authHeader != "Bearer my-secret-key" {
		t.Errorf("expected auth header 'Bearer my-secret-key', got '%s'", authHeader)
	}
}

func TestHandler_WithAttrs(t *testing.T) {
	var received map[string]any
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		json.Unmarshal(body, &received)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer server.Close()

	Config("test-key", server.URL)
	log := slog.New(NewHandler()).With("service", "api")

	log.Info("Request handled")
	Flush()

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	events := received["events"].([]any)
	ctx := events[0].(map[string]any)["context"].(map[string]any)

	if ctx["service"] != "api" {
		t.Errorf("expected service 'api', got %v", ctx["service"])
	}
}
