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

type Config struct {
	APIKey        string
	Endpoint      string
	BatchSize     int
	FlushInterval time.Duration
	MaxBufferSize int
}

type event struct {
	Message       string         `json:"message"`
	Timestamp     string         `json:"timestamp"`
	ErrorType     string         `json:"error_type,omitempty"`
	ErrorLocation string         `json:"error_location,omitempty"`
	Context       map[string]any `json:"context,omitempty"`
}

// shared state between handler and its WithAttrs/WithGroup clones
type state struct {
	mu            sync.Mutex
	buffer        []event
	flushTimer    *time.Timer
	backoffUntil  time.Time
	flushInterval time.Duration
}

type Handler struct {
	cfg    Config
	attrs  []slog.Attr
	groups []string
	state  *state
}

func NewHandler(cfg Config) *Handler {
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 10
	}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.MaxBufferSize == 0 {
		cfg.MaxBufferSize = 1000
	}

	h := &Handler{
		cfg: cfg,
		state: &state{
			buffer:        make([]event, 0, cfg.BatchSize),
			flushInterval: cfg.FlushInterval,
		},
	}

	// Auto-flush on shutdown signals
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		h.Flush()
	}()

	return h
}

func (h *Handler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	e := event{
		Message:   r.Message,
		Timestamp: r.Time.UTC().Format(time.RFC3339),
		Context:   make(map[string]any),
	}

	// Add handler-level attrs
	for _, a := range h.attrs {
		h.addAttr(e.Context, a)
	}

	// Add record attrs
	r.Attrs(func(a slog.Attr) bool {
		h.addAttr(e.Context, a)
		return true
	})

	// Extract error info if present
	if errVal, ok := e.Context["error"]; ok {
		if err, ok := errVal.(error); ok {
			e.Context["error"] = err.Error()
		}
	}
	if errType, ok := e.Context["error_type"].(string); ok {
		e.ErrorType = errType
		delete(e.Context, "error_type")
	}
	if errLoc, ok := e.Context["error_location"].(string); ok {
		e.ErrorLocation = errLoc
		delete(e.Context, "error_location")
	}

	// Errors sent immediately with retries
	if r.Level >= slog.LevelError {
		go h.send([]event{e}, 3)
		return nil
	}

	h.enqueue(e)
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)

	return &Handler{
		cfg:    h.cfg,
		attrs:  newAttrs,
		groups: h.groups,
		state:  h.state, // Share state
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	newGroups := make([]string, len(h.groups)+1)
	copy(newGroups, h.groups)
	newGroups[len(h.groups)] = name

	return &Handler{
		cfg:    h.cfg,
		attrs:  h.attrs,
		groups: newGroups,
		state:  h.state, // Share state
	}
}

func (h *Handler) addAttr(m map[string]any, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}

	val := a.Value.Resolve()
	if val.Kind() == slog.KindGroup {
		group := make(map[string]any)
		for _, ga := range val.Group() {
			h.addAttr(group, ga)
		}
		m[a.Key] = group
	} else {
		m[a.Key] = val.Any()
	}
}

func (h *Handler) enqueue(e event) {
	h.state.mu.Lock()
	defer h.state.mu.Unlock()

	// Buffer full - drop oldest non-error
	if len(h.state.buffer) >= h.cfg.MaxBufferSize {
		for i, ev := range h.state.buffer {
			if ev.ErrorType == "" {
				h.state.buffer = append(h.state.buffer[:i], h.state.buffer[i+1:]...)
				break
			}
		}
	}

	h.state.buffer = append(h.state.buffer, e)

	if h.state.flushTimer == nil {
		h.state.flushTimer = time.AfterFunc(h.state.flushInterval, func() {
			h.Flush()
		})
	}

	if len(h.state.buffer) >= h.cfg.BatchSize {
		go h.Flush()
	}
}

func (h *Handler) Flush() {
	h.state.mu.Lock()
	if len(h.state.buffer) == 0 {
		h.state.mu.Unlock()
		return
	}

	if h.state.flushTimer != nil {
		h.state.flushTimer.Stop()
		h.state.flushTimer = nil
	}

	events := h.state.buffer
	h.state.buffer = make([]event, 0, h.cfg.BatchSize)
	h.state.mu.Unlock()

	h.send(events, 1)
}

func (h *Handler) send(events []event, retries int) {
	if len(events) == 0 {
		return
	}

	// Respect backoff
	h.state.mu.Lock()
	if time.Now().Before(h.state.backoffUntil) {
		wait := time.Until(h.state.backoffUntil)
		h.state.mu.Unlock()
		time.Sleep(wait)
	} else {
		h.state.mu.Unlock()
	}

	body, _ := json.Marshal(map[string]any{"events": events})

	for attempt := 0; attempt <= retries; attempt++ {
		req, _ := http.NewRequest("POST", h.cfg.Endpoint+"/api/v1/events/batch", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+h.cfg.APIKey)

		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				// Success - recover flush interval
				h.state.mu.Lock()
				h.state.flushInterval = max(h.cfg.FlushInterval, h.state.flushInterval*9/10)
				h.state.mu.Unlock()
				return
			}
			if resp.StatusCode == 429 {
				// Server busy - back off
				h.state.mu.Lock()
				h.state.flushInterval = min(h.state.flushInterval*2, time.Minute)
				h.state.backoffUntil = time.Now().Add(h.state.flushInterval)
				h.state.mu.Unlock()
			}
		}

		if attempt < retries {
			time.Sleep(time.Second * time.Duration(1<<attempt))
		}
	}
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// Middleware returns HTTP middleware that logs requests
func Middleware(handler *Handler) func(http.Handler) http.Handler {
	log := slog.New(handler)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &responseWriter{ResponseWriter: w, status: 200}

			next.ServeHTTP(rw, r)

			duration := time.Since(start).Milliseconds()
			msg := fmt.Sprintf("%s %s → %d", r.Method, r.URL.Path, rw.status)

			if rw.status >= 500 {
				log.Error(msg,
					"method", r.Method,
					"path", r.URL.Path,
					"status", rw.status,
					"duration_ms", duration,
				)
			} else {
				log.Info(msg,
					"method", r.Method,
					"path", r.URL.Path,
					"status", rw.status,
					"duration_ms", duration,
				)
			}
		})
	}
}
