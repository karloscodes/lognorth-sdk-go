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
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

type event struct {
	Message   string         `json:"message"`
	Timestamp string         `json:"timestamp"`
	Context   map[string]any `json:"context,omitempty"`
}

var (
	mu       sync.Mutex
	apiKey   string
	endpoint string
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

// ErrorFields are the structured error fields added to context for error events.
// SDKs populate these automatically; the server uses them for three-tier issue grouping.
type ErrorFields struct {
	Error      string `json:"error"`
	ErrorClass string `json:"error_class"`
	ErrorFile  string `json:"error_file"`
	ErrorLine  int    `json:"error_line"`
	ErrorCaller string `json:"error_caller"`
	StackTrace string `json:"stack_trace"`
}

// Config sets the endpoint and API key. Call once at startup.
func Config(url, key string) {
	mu.Lock()
	defer mu.Unlock()
	endpoint = url
	apiKey = key
}

// Log sends a regular log message. Batched automatically.
func Log(message string, ctx map[string]any) {
	mu.Lock()
	buffer = append(buffer, event{
		Message:   message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Context:   ctx,
	})
	n := len(buffer)
	if timer == nil {
		timer = time.AfterFunc(5*time.Second, Flush)
	}
	mu.Unlock()

	if n >= 10 {
		go Flush()
	}
}

// Error sends an error log immediately.
func Error(message string, err error, ctx map[string]any) {
	if ctx == nil {
		ctx = make(map[string]any)
	}
	ctx["error"] = err.Error()

	// Extract error class from type
	errorClass := "error"
	if err != nil {
		t := reflect.TypeOf(err)
		if t != nil {
			errorClass = strings.TrimPrefix(t.String(), "*")
		}
	}
	ctx["error_class"] = errorClass

	// Get caller info
	if pc, file, line, ok := runtime.Caller(1); ok {
		ctx["error_file"] = filepath.Base(file)
		ctx["error_line"] = line
		if fn := runtime.FuncForPC(pc); fn != nil {
			parts := strings.Split(fn.Name(), ".")
			ctx["error_caller"] = parts[len(parts)-1]
		}
	}

	// Capture stack trace
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	ctx["stack_trace"] = string(buf[:n])

	go send([]event{{
		Message:   message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Context:   ctx,
	}}, true)
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

// Handler implements slog.Handler for integration with log/slog.
type Handler struct {
	attrs []slog.Attr
}

// NewHandler creates a new LogNorth slog handler.
func NewHandler() *Handler {
	return &Handler{}
}

func (h *Handler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	ctx := make(map[string]any)

	for _, a := range h.attrs {
		ctx[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			if err, ok := a.Value.Any().(error); ok {
				ctx["error"] = err.Error()
			} else {
				ctx["error"] = a.Value.Any()
			}
		} else {
			ctx[a.Key] = a.Value.Any()
		}
		return true
	})

	if r.Level >= slog.LevelError {
		errVal := ctx["error"]
		if errVal == nil {
			errVal = r.Message
		}
		Error(r.Message, fmt.Errorf("%v", errVal), ctx)
	} else {
		Log(r.Message, ctx)
	}
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{attrs: append(h.attrs, attrs...)}
}

func (h *Handler) WithGroup(string) slog.Handler { return h }

// Middleware logs HTTP requests.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: 200}

		next.ServeHTTP(rw, r)

		Log(fmt.Sprintf("%s %s → %d", r.Method, r.URL.Path, rw.status), map[string]any{
			"method":      r.Method,
			"path":        r.URL.Path,
			"status":      rw.status,
			"duration_ms": time.Since(start).Milliseconds(),
		})
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
