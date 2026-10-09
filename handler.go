package lognorth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
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
	requestKey
)

// request holds what a handler learns about its request while it runs, such
// as the signed-in user. The middleware reads it when the request ends.
type request struct {
	mu   sync.Mutex
	user string
}

// SetUser names the user of the current request: an ID, not an email. The
// request event carries it, and so do errors logged with the request's
// context, so an issue shows how many users it hit. Call it in a handler or
// an auth middleware that runs inside lognorth.Middleware; elsewhere it does
// nothing.
//
//	lognorth.SetUser(r.Context(), strconv.Itoa(user.ID))
func SetUser(ctx context.Context, id string) {
	if req, ok := ctx.Value(requestKey).(*request); ok {
		req.mu.Lock()
		req.user = id
		req.mu.Unlock()
	}
}

func userFromContext(ctx context.Context) string {
	req, ok := ctx.Value(requestKey).(*request)
	if !ok {
		return ""
	}
	req.mu.Lock()
	defer req.mu.Unlock()
	return req.user
}

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
	timer        *time.Timer
	ignoredPaths []string
	release      = releaseFromEnv()
)

// releaseEnv lists where deploy tools put the version that runs. The first
// one set wins.
var releaseEnv = []string{
	"LOGNORTH_RELEASE", "GIT_SHA", "GIT_COMMIT", "SOURCE_COMMIT", "KAMAL_VERSION",
	"RENDER_GIT_COMMIT", "HEROKU_SLUG_COMMIT", "SOURCE_VERSION", "RAILWAY_GIT_COMMIT_SHA", "VERCEL_GIT_COMMIT_SHA",
}

func releaseFromEnv() string {
	for _, k := range releaseEnv {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// client gives up on a connect after 5 seconds and on a request after 10.
var client = newClient()

func newClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{Timeout: 10 * time.Second, Transport: t}
}

func init() {
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		shutdown()
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
	endpoint = url
	apiKey = key
	mu.Unlock()
	announceRelease()
}

// Options configures the client with environment tagging and enable/disable
// behavior. Use Configure for richer setup; Config remains for the simple case.
type Options struct {
	URL    string
	APIKey string
	// Environment is stamped on every event's context (e.g. "production",
	// "staging", "preview"). Optional.
	Environment string
	// Release is the version that runs, such as a git SHA. Error events carry
	// it, so an issue shows the release it first appeared in. When empty, the
	// SDK reads LOGNORTH_RELEASE, GIT_SHA, KAMAL_VERSION, and the variables
	// that common hosts set.
	Release string
	// Enabled overrides the default. When nil, the SDK auto-disables only when
	// Environment is "development" or "test"; everything else (staging, preview,
	// qa, production, custom) opts in.
	Enabled *bool
}

// Configure sets the endpoint, key, environment, and enable flag in one call.
func Configure(opts Options) {
	mu.Lock()
	endpoint = opts.URL
	apiKey = opts.APIKey
	environment = opts.Environment
	release = opts.Release
	if release == "" {
		release = releaseFromEnv()
	}
	if opts.Enabled != nil {
		enabled = *opts.Enabled
	} else {
		enabled = opts.Environment != "development" && opts.Environment != "test"
	}
	mu.Unlock()
	announceRelease()
}

// announced is the release this process last said it started.
var announced string

// announceRelease logs "Release <version> started" once per release, when
// the client is configured. LogNorth takes the first start of a release as
// its deploy time and marks it on its charts.
func announceRelease() {
	mu.Lock()
	r := release
	if r == "" || r == announced || !enabled {
		mu.Unlock()
		return
	}
	announced = r
	mu.Unlock()
	logEvent("Release "+r+" started", map[string]any{"release": r}, "", 0)
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
	enqueue(e)
}

// Error queues an error event and starts sending it at once.
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

	enqueue(event{
		Message:   message,
		Timestamp: timestamp.UTC().Format("2006-01-02T15:04:05.000000Z"),
		TraceID:   traceID,
		Context:   ctx,
	})
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
	if user := userFromContext(c); user != "" {
		ctx["user"] = user
	}

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
		ctx := context.WithValue(withTraceID(r.Context(), traceID), requestKey, &request{})
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
		if user := userFromContext(ctx); user != "" {
			eventCtx["user"] = user
		}
		// The user agent tells a bot from a browser on a failed request.
		// Only failed ones carry it, to keep every other event small.
		if rw.status >= 500 && r.UserAgent() != "" {
			eventCtx["user_agent"] = r.UserAgent()
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
