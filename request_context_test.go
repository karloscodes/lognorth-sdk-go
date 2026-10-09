package lognorth

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// collect starts a fake LogNorth server and returns a function that flushes
// the SDK and returns the context of every event the server got.
func collect(t *testing.T, opts Options) func() []map[string]any {
	t.Helper()
	var mu sync.Mutex
	var events []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var batch struct {
			Events []struct {
				Context map[string]any `json:"context"`
			} `json:"events"`
		}
		json.Unmarshal(body, &batch)
		mu.Lock()
		for _, e := range batch.Events {
			events = append(events, e.Context)
		}
		mu.Unlock()
	}))
	t.Cleanup(server.Close)
	opts.URL, opts.APIKey = server.URL, "k"
	Configure(opts)
	t.Cleanup(func() { Configure(Options{URL: server.URL, APIKey: "k"}) })

	return func() []map[string]any {
		Flush()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return events
	}
}

func TestRequestContext(t *testing.T) {
	t.Run("the request event carries the user the handler set", func(t *testing.T) {
		events := collect(t, Options{})
		handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			SetUser(r.Context(), "42")
		}))

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/account", nil))

		got := events()
		if len(got) != 1 || got[0]["user"] != "42" {
			t.Fatalf("expected one event with user 42, got %v", got)
		}
	})

	t.Run("an error logged with the request context carries the user", func(t *testing.T) {
		events := collect(t, Options{})
		logger := slog.New(NewHandler())
		handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			SetUser(r.Context(), "42")
			logger.ErrorContext(r.Context(), "charge failed", "error", errors.New("card declined"))
		}))

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/charge", nil))

		got := events()
		for _, e := range got {
			if e["error"] == "card declined" {
				if e["user"] != "42" {
					t.Fatalf("expected the error to carry user 42, got %v", e)
				}
				return
			}
		}
		t.Fatalf("expected an error event, got %v", got)
	})

	t.Run("a failed request carries the user agent and a good one does not", func(t *testing.T) {
		events := collect(t, Options{})
		handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/broken" {
				w.WriteHeader(500)
			}
		}))
		for _, path := range []string{"/ok", "/broken"} {
			req := httptest.NewRequest("GET", path, nil)
			req.Header.Set("User-Agent", "Mozilla/5.0")
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}

		got := events()
		if len(got) != 2 {
			t.Fatalf("expected 2 events, got %v", got)
		}
		for _, e := range got {
			if e["path"] == "/ok" && e["user_agent"] != nil {
				t.Errorf("expected no user agent on a good request, got %v", e)
			}
			if e["path"] == "/broken" && e["user_agent"] != "Mozilla/5.0" {
				t.Errorf("expected the user agent on a failed request, got %v", e)
			}
		}
	})

	t.Run("SetUser outside the middleware does nothing", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)

		SetUser(req.Context(), "42")

		if user := userFromContext(req.Context()); user != "" {
			t.Fatalf("expected no user, got %q", user)
		}
	})
}

func TestRelease(t *testing.T) {
	t.Run("errors carry the release and other events do not", func(t *testing.T) {
		events := collect(t, Options{Release: "a1b2c3d"})

		Log("signed up", nil)
		Error("charge failed", errors.New("card declined"), nil)

		got := events()
		if len(got) != 2 {
			t.Fatalf("expected 2 events, got %v", got)
		}
		for _, e := range got {
			if e["error"] == nil && e["release"] != nil {
				t.Errorf("expected no release on a log event, got %v", e)
			}
			if e["error"] != nil && e["release"] != "a1b2c3d" {
				t.Errorf("expected release a1b2c3d on the error, got %v", e)
			}
		}
	})

	t.Run("the release comes from the environment when not set", func(t *testing.T) {
		t.Setenv("KAMAL_VERSION", "f00ba44")
		events := collect(t, Options{})

		Error("charge failed", errors.New("card declined"), nil)

		got := events()
		if len(got) != 1 || got[0]["release"] != "f00ba44" {
			t.Fatalf("expected release f00ba44, got %v", got)
		}
	})
}
