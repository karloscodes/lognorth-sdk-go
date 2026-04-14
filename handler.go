package lognorth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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

const maxBuffer = 1000

type event struct {
	Message    string         `json:"message"`
	Timestamp  string         `json:"timestamp"`
	DurationMS int            `json:"duration_ms,omitempty"`
	TraceID    string         `json:"trace_id,omitempty"`
	Context    map[string]any `json:"context,omitempty"`
}

type ctxKey int

const (
	traceIDKey ctxKey = iota
	routeKey
	handlerKey
)

func withTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceIDKey, traceID)
}

func traceIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey).(string); ok {
		return v
	}
	return ""
}

// WithRoute returns a context with route info stamped on it. Call this inside
// your handler (or router-specific middleware) so the request event carries
// the route pattern and the handler function name:
//
//	// chi example:
//	r.Use(func(next http.Handler) http.Handler {
//	    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	        rc := chi.RouteContext(r.Context())
//	        ctx := lognorth.WithRoute(r.Context(), rc.RoutePattern(), "")
//	        next.ServeHTTP(w, r.WithContext(ctx))
//	    })
//	})
//
// Empty strings are ignored. Standard library Go 1.22+ ServeMux users don't
// need this — lognorth.Middleware reads r.Pattern automatically.
func WithRoute(ctx context.Context, route, handler string) context.Context {
	if route != "" {
		ctx = context.WithValue(ctx, routeKey, route)
	}
	if handler != "" {
		ctx = context.WithValue(ctx, handlerKey, handler)
	}
	return ctx
}

func routeFromContext(ctx context.Context) (route, handler string) {
	if v, ok := ctx.Value(routeKey).(string); ok {
		route = v
	}
	if v, ok := ctx.Value(handlerKey).(string); ok {
		handler = v
	}
	return
}

func generateTraceID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var (
	mu           sync.Mutex
	apiKey       string
	endpoint     string
	environment  string
	enabled      = true
	buffer       []event
	timer        *time.Timer
	backoff      time.Time
	ignoredPaths []string
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
	Error       string `json:"error"`
	ErrorClass  string `json:"error_class"`
	ErrorFile   string `json:"error_file"`
	ErrorLine   int    `json:"error_line"`
	ErrorCaller string `json:"error_caller"`
	StackTrace  string `json:"stack_trace"`
}

// Config sets the endpoint and API key. Call once at startup.
func Config(url, key string) {
	mu.Lock()
	defer mu.Unlock()
	endpoint = url
	apiKey = key
}

// Options configures the client with environment tagging and enable/disable
// behavior. Use Configure for richer setup; Config remains for the simple case.
type Options struct {
	URL    string
	APIKey string
	// Environment is stamped on every event's context (e.g. "production",
	// "staging", "preview"). Optional.
	Environment string
	// Enabled overrides the default. When nil, the SDK auto-disables only when
	// Environment is "development" or "test"; everything else (staging, preview,
	// qa, production, custom) opts in.
	Enabled *bool
}

// Configure sets the endpoint, key, environment, and enable flag in one call.
func Configure(opts Options) {
	mu.Lock()
	defer mu.Unlock()
	endpoint = opts.URL
	apiKey = opts.APIKey
	environment = opts.Environment
	if opts.Enabled != nil {
		enabled = *opts.Enabled
	} else {
		enabled = opts.Environment != "development" && opts.Environment != "test"
	}
}

// SetEnvironment changes the environment label after Configure/Config.
func SetEnvironment(env string) {
	mu.Lock()
	defer mu.Unlock()
	environment = env
}

// SetEnabled toggles whether events are sent. When false, all log/error calls
// become no-ops (no buffer, no HTTP).
func SetEnabled(b bool) {
	mu.Lock()
	defer mu.Unlock()
	enabled = b
}

func stampEnvironment(ctx map[string]any) map[string]any {
	mu.Lock()
	env := environment
	mu.Unlock()
	if env == "" {
		return ctx
	}
	if ctx == nil {
		ctx = make(map[string]any)
	}
	ctx["environment"] = env
	return ctx
}

func isEnabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return enabled
}

// IgnorePaths sets paths that should not be logged by the middleware.
// Useful for health checks and metrics endpoints.
//
//	lognorth.IgnorePaths("/healthz", "/_health", "/metrics")
func IgnorePaths(paths ...string) {
	mu.Lock()
	defer mu.Unlock()
	ignoredPaths = paths
}

func isIgnoredPath(path string) bool {
	mu.Lock()
	defer mu.Unlock()
	for _, p := range ignoredPaths {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// Log sends a regular log message. Batched automatically.
// For trace ID propagation inside HTTP handlers, use slog with NewHandler() instead:
//
//	slog.InfoContext(r.Context(), "listing users", "page", 1)
func Log(message string, ctx map[string]any) {
	logEvent(message, ctx, "", 0)
}

func logEvent(message string, ctx map[string]any, traceID string, durationMS int, ts ...time.Time) {
	if !isEnabled() {
		return
	}
	timestamp := time.Now()
	if len(ts) > 0 && !ts[0].IsZero() {
		timestamp = ts[0]
	}
	e := event{
		Message:    message,
		Timestamp:  timestamp.UTC().Format("2006-01-02T15:04:05.000000Z"),
		TraceID:    traceID,
		DurationMS: durationMS,
		Context:    stampEnvironment(ctx),
	}
	mu.Lock()
	buffer = append(buffer, e)
	if len(buffer) > maxBuffer {
		buffer = buffer[len(buffer)-maxBuffer:]
	}
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
// For trace ID propagation inside HTTP handlers, use slog with NewHandler() instead:
//
//	slog.ErrorContext(r.Context(), "query failed", "error", err)
func Error(message string, err error, ctx map[string]any) {
	errorEvent(message, err, ctx, "", 2)
}

func errorEvent(message string, err error, ctx map[string]any, traceID string, callerSkip int, ts ...time.Time) {
	if !isEnabled() {
		return
	}
	timestamp := time.Now()
	if len(ts) > 0 && !ts[0].IsZero() {
		timestamp = ts[0]
	}
	if ctx == nil {
		ctx = make(map[string]any)
	}
	ctx = stampEnvironment(ctx)
	ctx["error"] = err.Error()

	errorClass := "error"
	if err != nil {
		t := reflect.TypeOf(err)
		if t != nil {
			errorClass = strings.TrimPrefix(t.String(), "*")
		}
	}
	ctx["error_class"] = errorClass

	if pc, file, line, ok := runtime.Caller(callerSkip); ok {
		ctx["error_file"] = filepath.Base(file)
		ctx["error_line"] = line
		if fn := runtime.FuncForPC(pc); fn != nil {
			parts := strings.Split(fn.Name(), ".")
			ctx["error_caller"] = parts[len(parts)-1]
		}
	}

	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	ctx["stack_trace"] = string(buf[:n])

	go send([]event{{
		Message:   message,
		Timestamp: timestamp.UTC().Format("2006-01-02T15:04:05.000000Z"),
		TraceID:   traceID,
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

func requeue(events []event) {
	mu.Lock()
	buffer = append(events, buffer...)
	if len(buffer) > maxBuffer {
		buffer = buffer[:maxBuffer]
	}
	mu.Unlock()
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
			requeue(events)
		}
		return
	}
	resp.Body.Close()

	if resp.StatusCode == 429 {
		mu.Lock()
		backoff = time.Now().Add(5 * time.Second)
		mu.Unlock()
		requeue(events)
	}
}

// Logger wraps a context for convenient slog calls with trace ID propagation.
// Use From(ctx) inside HTTP handlers, then call Info/Error without passing context each time.
type Logger struct {
	ctx context.Context
}

// From creates a Logger bound to the given context.
// The trace ID from the middleware is carried automatically.
func From(ctx context.Context) Logger {
	return Logger{ctx: ctx}
}

// Info logs a message at info level with the bound context.
func (l Logger) Info(msg string, args ...any) {
	slog.Default().InfoContext(l.ctx, msg, args...)
}

// Error logs a message at error level with the bound context.
func (l Logger) Error(msg string, args ...any) {
	slog.Default().ErrorContext(l.ctx, msg, args...)
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

func (h *Handler) Handle(c context.Context, r slog.Record) error {
	ctx := make(map[string]any)
	traceID := traceIDFromContext(c)

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
		errorEvent(r.Message, fmt.Errorf("%v", errVal), ctx, traceID, 4)
	} else {
		logEvent(r.Message, ctx, traceID, 0)
	}
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{attrs: append(h.attrs, attrs...)}
}

func (h *Handler) WithGroup(string) slog.Handler { return h }

// Middleware logs HTTP requests with trace_id propagation.
// Paths set via IgnorePaths() will not be logged.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if this path should be ignored
		if isIgnoredPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: 200}

		traceID := r.Header.Get("X-Trace-ID")
		if traceID == "" {
			traceID = generateTraceID()
		}
		w.Header().Set("X-Trace-ID", traceID)
		ctx := withTraceID(r.Context(), traceID)
		r = r.WithContext(ctx)

		next.ServeHTTP(rw, r)

		eventCtx := map[string]any{"method": r.Method, "path": r.URL.Path, "status": rw.status}

		// Prefer the Go 1.22+ ServeMux pattern if set; otherwise fall back
		// to whatever the user stamped via WithRoute (chi, echo, etc.).
		route, handlerName := routeFromContext(r.Context())
		if r.Pattern != "" {
			route = r.Pattern
		}
		if route != "" {
			eventCtx["route"] = route
		}
		if handlerName != "" {
			eventCtx["handler"] = handlerName
		}

		logEvent(
			fmt.Sprintf("%s %s → %d", r.Method, r.URL.Path, rw.status),
			eventCtx,
			traceID,
			int(time.Since(start).Milliseconds()),
			start,
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
