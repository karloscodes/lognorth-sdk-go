package lognorth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	minBackoff = 10 * time.Millisecond
	maxBackoff = 80 * time.Millisecond
	minConfigWait = 20 * time.Millisecond
	maxConfigWait = 80 * time.Millisecond
	retryAfterUnit = 10 * time.Millisecond
	shutdownTimeout = 500 * time.Millisecond
	os.Exit(m.Run())
}

type answerFunc func(n int, events []map[string]any) (status int, retryAfter string)

func accept(int, []map[string]any) (int, string) { return 201, "" }

// collector is a LogNorth server that records every request.
type collector struct {
	mu        sync.Mutex
	requests  [][]map[string]any
	times     []time.Time
	sizes     []int
	delivered []map[string]any
}

func newCollector(t *testing.T, answer answerFunc) *collector {
	c := &collector{}
	srv := httptest.NewServer(c.handler(answer))
	t.Cleanup(srv.Close)
	reset(t, srv.URL)
	return c
}

func (c *collector) handler(answer answerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var data struct {
			Events []map[string]any `json:"events"`
		}
		json.Unmarshal(body, &data)
		c.mu.Lock()
		c.requests = append(c.requests, data.Events)
		c.times = append(c.times, time.Now())
		c.sizes = append(c.sizes, len(body))
		n := len(c.requests)
		c.mu.Unlock()

		status, retryAfter := answer(n, data.Events)
		if status/100 == 2 {
			c.mu.Lock()
			c.delivered = append(c.delivered, data.Events...)
			c.mu.Unlock()
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
	})
}

func (c *collector) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.delivered {
		out = append(out, e["message"].(string))
	}
	return out
}

func (c *collector) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func (c *collector) deliveredCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.delivered)
}

func (c *collector) last() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.delivered[len(c.delivered)-1]
}

// reset clears the delivery state, points the SDK at url, and captures
// stderr. It clears the state again when the test ends.
func reset(t *testing.T, url string) {
	t.Helper()
	clearState()
	Configure(Options{URL: url, APIKey: "k"})
	t.Cleanup(clearState)
}

func clearState() {
	sendMu.Lock()
	defer sendMu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	queue, queueBytes = nil, 0
	dropped, droppedErrors = 0, 0
	retryAt, backoff, configWait = time.Time{}, 0, 0
	batchLimit = maxBatchEvents
	failing = ""
	if timer != nil {
		timer.Stop()
		timer = nil
	}
	stderr = &bytes.Buffer{}
}

func stderrText() string {
	mu.Lock()
	defer mu.Unlock()
	return stderr.(*bytes.Buffer).String()
}

func queued() int {
	mu.Lock()
	defer mu.Unlock()
	return len(queue)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// gate blocks the first request until release is called, so the test can
// fill the queue while the server is down.
func gate(t *testing.T, then answerFunc) (answerFunc, func()) {
	ch := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return func(n int, events []map[string]any) (int, string) {
		if n == 1 {
			<-ch
			return 500, ""
		}
		return then(n, events)
	}, release
}

func TestRetryAfter(t *testing.T) {
	for _, status := range []int{503, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := newCollector(t, func(n int, _ []map[string]any) (int, string) {
				if n == 1 {
					return status, "3"
				}
				return 201, ""
			})

			Log("a", nil)
			Log("b", nil)
			Flush()
			waitFor(t, "delivery", func() bool { return c.deliveredCount() == 2 })

			if got := c.messages(); fmt.Sprint(got) != "[a b]" {
				t.Errorf("expected [a b] once, got %v", got)
			}
			if c.requestCount() != 2 {
				t.Errorf("expected 2 requests, got %d", c.requestCount())
			}
			if gap := c.times[1].Sub(c.times[0]); gap < 30*time.Millisecond {
				t.Errorf("expected a wait of at least 30ms from Retry-After, got %v", gap)
			}
		})
	}
}

func TestRetryAfterDoesNotGrowTheBackoff(t *testing.T) {
	c := newCollector(t, func(n int, _ []map[string]any) (int, string) {
		switch {
		case n <= 4:
			return 503, "1"
		case n == 5:
			return 500, ""
		}
		return 201, ""
	})

	Log("a", nil)
	Flush()
	waitFor(t, "delivery", func() bool { return c.deliveredCount() == 1 })

	if gap := c.times[5].Sub(c.times[4]); gap > 40*time.Millisecond {
		t.Errorf("expected the first backoff of about 10ms after the 500, got %v", gap)
	}
}

func TestServerErrorIsRetriedWithBackoff(t *testing.T) {
	c := newCollector(t, func(n int, _ []map[string]any) (int, string) {
		if n <= 2 {
			return 500, ""
		}
		return 201, ""
	})

	Log("a", nil)
	Flush()
	waitFor(t, "delivery", func() bool { return c.deliveredCount() == 1 })

	if c.requestCount() != 3 {
		t.Fatalf("expected 3 requests, got %d", c.requestCount())
	}
	first, second := c.times[1].Sub(c.times[0]), c.times[2].Sub(c.times[1])
	if first < 8*time.Millisecond || second < 16*time.Millisecond {
		t.Errorf("expected backoff of about 10ms then 20ms, got %v then %v", first, second)
	}
}

func TestNetworkFailureKeepsLogEvents(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	reset(t, "http://"+addr)

	Log("one", nil)
	Log("two", nil)
	Flush()
	time.Sleep(30 * time.Millisecond)
	waitFor(t, "2 events kept while the server is down", func() bool { return queued() == 2 })

	c := &collector{}
	srv := httptest.NewUnstartedServer(c.handler(accept))
	srv.Listener.Close()
	srv.Listener, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv.Start()
	defer srv.Close()
	waitFor(t, "delivery", func() bool { return c.deliveredCount() == 2 })

	if got := c.messages(); fmt.Sprint(got) != "[one two]" {
		t.Errorf("expected [one two], got %v", got)
	}
}

func TestPayloadTooLarge(t *testing.T) {
	t.Run("splits the batch and delivers every event", func(t *testing.T) {
		c := newCollector(t, func(_ int, events []map[string]any) (int, string) {
			if len(events) > 4 {
				return 413, ""
			}
			return 201, ""
		})

		for i := range 10 {
			Log(fmt.Sprint(i), nil)
		}
		waitFor(t, "delivery", func() bool { return c.deliveredCount() == 10 })

		if got := c.messages(); fmt.Sprint(got) != "[0 1 2 3 4 5 6 7 8 9]" {
			t.Errorf("expected all 10 events in order, got %v", got)
		}
	})

	t.Run("drops and counts a single event", func(t *testing.T) {
		c := newCollector(t, func(_ int, events []map[string]any) (int, string) {
			if events[0]["message"] == "huge" {
				return 413, ""
			}
			return 201, ""
		})

		Log("huge", nil)
		Flush()
		Log("next", nil)
		Flush()
		waitFor(t, "drop report", func() bool { return c.deliveredCount() == 2 })

		if got := c.messages(); fmt.Sprint(got) != "[next LogNorth client dropped 1 events]" {
			t.Errorf("expected next and the drop report, got %v", got)
		}
		ctx := c.last()["context"].(map[string]any)
		if ctx["dropped"] != float64(1) || ctx["dropped_errors"] != float64(0) {
			t.Errorf("expected dropped 1 and dropped_errors 0, got %v", ctx)
		}
	})
}

func TestUnauthorizedKeepsEvents(t *testing.T) {
	var fixed atomic.Bool
	c := newCollector(t, func(int, []map[string]any) (int, string) {
		if fixed.Load() {
			return 201, ""
		}
		return 401, ""
	})

	Log("a", nil)
	Log("b", nil)
	Flush()
	waitFor(t, "retries", func() bool { return c.requestCount() >= 3 })

	waitFor(t, "2 events kept", func() bool { return queued() == 2 })
	if c.deliveredCount() != 0 {
		t.Fatalf("expected no delivery on 401, got %d events", c.deliveredCount())
	}
	if n := strings.Count(stderrText(), "HTTP 401"); n != 1 {
		t.Errorf("expected one stderr line about HTTP 401, got %d:\n%s", n, stderrText())
	}

	fixed.Store(true)
	waitFor(t, "delivery", func() bool { return c.deliveredCount() == 2 })
	waitFor(t, "a recovered line", func() bool { return strings.Contains(stderrText(), "delivering events again") })
}

func TestQueueLimits(t *testing.T) {
	t.Run("event limit drops the oldest log events and keeps errors", func(t *testing.T) {
		answer, release := gate(t, accept)
		c := newCollector(t, answer)

		Log("log 0", nil)
		wake()
		waitFor(t, "first request", func() bool { return c.requestCount() == 1 })
		for i := 1; i <= 10100; i++ {
			if i%100 == 0 {
				Log(fmt.Sprintf("error %d", i), map[string]any{"error_class": "E"})
			} else {
				Log(fmt.Sprintf("log %d", i), nil)
			}
		}
		release()
		waitFor(t, "drop report", func() bool { return c.deliveredCount() == 10001 })

		ctx := c.last()["context"].(map[string]any)
		if ctx["dropped"] != float64(101) || ctx["dropped_errors"] != float64(0) {
			t.Errorf("expected dropped 101 and dropped_errors 0, got %v", ctx)
		}
		got := c.messages()
		if got[0] != "error 100" || got[1] != "log 102" || got[9998] != "log 10099" {
			t.Errorf("expected the oldest log events dropped, got %v ... %v", got[:3], got[9997:])
		}
		if errors := strings.Count(strings.Join(got, ","), "error "); errors != 101 {
			t.Errorf("expected all 101 errors delivered, got %d", errors)
		}
	})

	t.Run("byte limit drops the oldest log events and keeps errors", func(t *testing.T) {
		answer, release := gate(t, accept)
		c := newCollector(t, answer)
		pad := map[string]any{}
		for i := range 7 {
			pad[fmt.Sprint("pad", i)] = strings.Repeat("x", 8000)
		}
		withError := map[string]any{"error": "boom"}
		for k, v := range pad {
			withError[k] = v
		}

		Log("first", nil)
		wake()
		waitFor(t, "first request", func() bool { return c.requestCount() == 1 })
		for i := range 200 {
			if i%40 == 0 {
				Log(fmt.Sprint("error ", i), withError)
			} else {
				Log(fmt.Sprint("log ", i), pad)
			}
		}
		release()
		waitFor(t, "drop report", func() bool {
			return c.deliveredCount() > 0 && strings.HasPrefix(c.messages()[c.deliveredCount()-1], "LogNorth client dropped")
		})

		ctx := c.last()["context"].(map[string]any)
		dropped := int(ctx["dropped"].(float64))
		if dropped < 10 || ctx["dropped_errors"] != float64(0) {
			t.Errorf("expected about 14 log events dropped and no errors, got %v", ctx)
		}
		if delivered := c.deliveredCount() - 1; delivered+dropped != 201 {
			t.Errorf("expected delivered plus dropped to be 201, got %d + %d", delivered, dropped)
		}
		if errors := strings.Count(strings.Join(c.messages(), ","), "error "); errors != 5 {
			t.Errorf("expected all 5 errors delivered, got %d", errors)
		}
	})

	t.Run("only errors drops the oldest errors", func(t *testing.T) {
		answer, release := gate(t, accept)
		c := newCollector(t, answer)

		for i := range 10050 {
			Log(fmt.Sprint("error ", i), map[string]any{"status": 500})
			if i == 0 {
				waitFor(t, "first request", func() bool { return c.requestCount() == 1 })
			}
		}
		release()
		waitFor(t, "drop report", func() bool { return c.deliveredCount() == 10001 })

		ctx := c.last()["context"].(map[string]any)
		if ctx["dropped"] != float64(50) || ctx["dropped_errors"] != float64(50) {
			t.Errorf("expected dropped 50 and dropped_errors 50, got %v", ctx)
		}
		if got := c.messages(); got[0] != "error 50" {
			t.Errorf("expected the oldest errors dropped, got first %q", got[0])
		}
	})
}

func TestTrimming(t *testing.T) {
	t.Run("cuts long strings and marks the event", func(t *testing.T) {
		c := newCollector(t, accept)
		ctx := map[string]any{
			"blob":        strings.Repeat("b", 100_000),
			"stack_trace": "top of stack\n" + strings.Repeat("s", 50_000),
		}

		Log(strings.Repeat("m", 1500), ctx)
		Flush()
		waitFor(t, "delivery", func() bool { return c.deliveredCount() == 1 })

		e := c.last()
		got := e["context"].(map[string]any)
		if len(e["message"].(string)) != 1000 {
			t.Errorf("expected message of 1000 characters, got %d", len(e["message"].(string)))
		}
		if len(got["blob"].(string)) != 8<<10 {
			t.Errorf("expected blob of 8 KB, got %d", len(got["blob"].(string)))
		}
		stack := got["stack_trace"].(string)
		if len(stack) != 16<<10 || !strings.HasPrefix(stack, "top of stack") {
			t.Errorf("expected the top 16 KB of the stack, got %d bytes", len(stack))
		}
		if got["truncated"] != true {
			t.Errorf("expected truncated true, got %v", got["truncated"])
		}
		if len(ctx["blob"].(string)) != 100_000 {
			t.Error("expected the caller's context to stay unchanged")
		}
	})

	t.Run("keeps a minimal context when still too big", func(t *testing.T) {
		c := newCollector(t, accept)
		ctx := map[string]any{"method": "GET", "status": 200}
		for i := range 10 {
			ctx[fmt.Sprint("field", i)] = strings.Repeat("x", 8000)
		}

		Log("big", ctx)
		Flush()
		waitFor(t, "delivery", func() bool { return c.deliveredCount() == 1 })

		got := c.last()["context"].(map[string]any)
		if len(got) != 3 || got["method"] != "GET" || got["status"] != float64(200) || got["truncated"] != true {
			t.Errorf("expected only method, status, and truncated, got %v", got)
		}
	})
}

func TestBatchLimits(t *testing.T) {
	answer, release := gate(t, accept)
	c := newCollector(t, answer)
	pad := map[string]any{}
	for i := range 7 {
		pad[fmt.Sprint("pad", i)] = strings.Repeat("x", 8000)
	}

	Log("first", nil)
	wake()
	waitFor(t, "first request", func() bool { return c.requestCount() == 1 })
	for i := range 1200 {
		Log(fmt.Sprint("small ", i), nil)
	}
	for i := range 40 {
		Log(fmt.Sprint("big ", i), pad)
	}
	release()
	waitFor(t, "delivery", func() bool { return c.deliveredCount() == 1241 })

	c.mu.Lock()
	defer c.mu.Unlock()
	full := 0
	for i, events := range c.requests {
		if len(events) > 500 || c.sizes[i] > 1<<20 {
			t.Errorf("request %d holds %d events and %d bytes", i, len(events), c.sizes[i])
		}
		if len(events) == 500 {
			full++
		}
	}
	if full != 2 {
		t.Errorf("expected 2 batches of 500 events, got %d", full)
	}
}

func TestOrderAcrossRetries(t *testing.T) {
	c := newCollector(t, func(n int, _ []map[string]any) (int, string) {
		switch n {
		case 1, 2:
			return 503, ""
		case 3:
			return 500, ""
		}
		return 201, ""
	})

	var want []string
	for i := range 25 {
		msg := fmt.Sprint(i)
		want = append(want, msg)
		if i%7 == 0 {
			Error(msg, fmt.Errorf("boom"), nil)
		} else {
			Log(msg, nil)
		}
	}
	Flush()
	waitFor(t, "delivery", func() bool { return c.deliveredCount() >= 25 })

	if got := c.messages(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestEventsLoggedDuringBackoff(t *testing.T) {
	c := newCollector(t, func(n int, _ []map[string]any) (int, string) {
		if n == 1 {
			return 503, "5"
		}
		return 201, ""
	})

	Log("a", nil)
	Flush()
	Log("b", nil)
	Error("c", fmt.Errorf("boom"), nil)
	if c.requestCount() != 1 {
		t.Fatalf("expected no send during the backoff, got %d requests", c.requestCount())
	}
	waitFor(t, "delivery", func() bool { return c.deliveredCount() == 3 })

	if got := c.messages(); fmt.Sprint(got) != "[a b c]" {
		t.Errorf("expected [a b c], got %v", got)
	}
}

func TestSendsAfterFlushInterval(t *testing.T) {
	c := newCollector(t, accept)
	flushInterval = 20 * time.Millisecond
	defer func() { flushInterval = 5 * time.Second }()

	Log("a", nil)
	waitFor(t, "delivery", func() bool { return c.deliveredCount() == 1 })
}

func TestShutdown(t *testing.T) {
	t.Run("sends buffered events", func(t *testing.T) {
		c := newCollector(t, accept)

		Log("a", nil)
		Log("b", nil)
		shutdown()

		if got := c.messages(); fmt.Sprint(got) != "[a b]" {
			t.Errorf("expected [a b] before shutdown returns, got %v", got)
		}
	})

	t.Run("reports events it cannot send", func(t *testing.T) {
		newCollector(t, func(int, []map[string]any) (int, string) { return 503, "" })

		Log("a", nil)
		Log("b", nil)
		shutdown()

		if !strings.Contains(stderrText(), "dropped 2 events at shutdown") {
			t.Errorf("expected a shutdown drop line, got:\n%s", stderrText())
		}
	})
}
