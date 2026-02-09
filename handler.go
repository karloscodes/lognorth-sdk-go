package lognorth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type event struct {
	Message   string         `json:"message"`
	Timestamp string         `json:"timestamp"`
	ErrorType string         `json:"error_type,omitempty"`
	Context   map[string]any `json:"context,omitempty"`
}

var (
	mu       sync.Mutex
	apiKey   = os.Getenv("LOGNORTH_API_KEY")
	endpoint = os.Getenv("LOGNORTH_URL")
	buffer   []event
	timer    *time.Timer
	backoff  time.Time
)

func init() {
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		Flush()
		os.Exit(0)
	}()
}

// Config sets the API key and endpoint. Optional - reads from env vars by default.
func Config(key, ep string) {
	mu.Lock()
	defer mu.Unlock()
	if key != "" {
		apiKey = key
	}
	if ep != "" {
		endpoint = ep
	}
}

func send(events []event, isError bool) {
	if len(events) == 0 || endpoint == "" {
		return
	}

	mu.Lock()
	if time.Now().Before(backoff) {
		mu.Unlock()
		return
	}
	mu.Unlock()

	body, _ := json.Marshal(map[string]any{"events": events})
	req, _ := http.NewRequest("POST", endpoint+"/api/v1/events/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if isError {
			mu.Lock()
			buffer = append(events, buffer...)
			mu.Unlock()
		}
		return
	}
	resp.Body.Close()

	if resp.StatusCode == 429 {
		mu.Lock()
		backoff = time.Now().Add(5 * time.Second)
		if !isError {
			buffer = append(events, buffer...)
		}
		mu.Unlock()
	}
}

// Flush sends all buffered events.
func Flush() {
	mu.Lock()
	if timer != nil {
		timer.Stop()
		timer = nil
	}
	if len(buffer) == 0 {
		mu.Unlock()
		return
	}
	events := buffer
	buffer = nil
	mu.Unlock()

	send(events, false)
}

func schedule() {
	if timer == nil {
		timer = time.AfterFunc(5*time.Second, Flush)
	}
}

// Handler implements slog.Handler
type Handler struct {
	attrs []slog.Attr
}

// NewHandler creates a new LogNorth slog handler.
func NewHandler() *Handler {
	return &Handler{}
}

func (h *Handler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	e := event{
		Message:   r.Message,
		Timestamp: r.Time.UTC().Format(time.RFC3339),
		Context:   make(map[string]any),
	}

	for _, a := range h.attrs {
		e.Context[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			if err, ok := a.Value.Any().(error); ok {
				e.Context["error"] = err.Error()
			} else {
				e.Context["error"] = a.Value.Any()
			}
		} else {
			e.Context[a.Key] = a.Value.Any()
		}
		return true
	})

	if r.Level >= slog.LevelError {
		e.ErrorType = "Error"
		go send([]event{e}, true)
		return nil
	}

	mu.Lock()
	buffer = append(buffer, e)
	n := len(buffer)
	schedule()
	mu.Unlock()

	if n >= 10 {
		go Flush()
	}
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{attrs: append(h.attrs, attrs...)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return h // Groups not supported for simplicity
}

// Middleware logs HTTP requests.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)

		slog.Info(fmt.Sprintf("%s %s → %d", r.Method, r.URL.Path, rw.status),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
