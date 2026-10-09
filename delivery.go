package lognorth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// Queue and batch limits. The server accepts up to 1,000 events and 4 MB a
// request, so batches stay well under that.
const (
	maxQueueEvents  = 10000
	maxQueueBytes   = 10 << 20
	maxEventBytes   = 64 << 10
	maxMessageChars = 1000
	maxStackBytes   = 16 << 10
	maxStringBytes  = 8 << 10
	maxBatchEvents  = 500
	maxBatchBytes   = 1 << 20
	wakeEvents      = 10
)

// Wait times. Tests shorten them.
var (
	flushInterval   = 5 * time.Second
	minBackoff      = 1 * time.Second
	maxBackoff      = 60 * time.Second
	minConfigWait   = 60 * time.Second
	maxConfigWait   = 300 * time.Second
	retryAfterUnit  = time.Second
	shutdownTimeout = 5 * time.Second
)

// stderr receives one line per change of state. Guarded by mu.
var stderr io.Writer = os.Stderr

// minimalContext lists the context keys kept when an event is still too big
// after trimming its strings.
var minimalContext = []string{"error", "error_class", "error_file", "error_line", "method", "path", "status", "environment", "release", "user"}

// item is one queued event, stored as the JSON the server receives.
type item struct {
	raw   []byte
	isErr bool
}

// Delivery state. Everything except sendMu is guarded by mu.
var (
	queue         []item
	queueBytes    int
	dropped       int
	droppedErrors int
	sending       bool
	retryAt       time.Time
	backoff       time.Duration
	configWait    time.Duration
	batchLimit    = maxBatchEvents
	failing       string

	// sendMu keeps one request in flight. Hold it before mu.
	sendMu sync.Mutex
)

// enqueue trims the event, adds it to the queue, and wakes the sender when
// a batch is due. It never blocks on the network.
func enqueue(e event) {
	isErr := isError(e.Context)
	if isErr {
		e.Context = stampRelease(e.Context)
	}
	raw, ok := trim(&e)

	mu.Lock()
	if !ok {
		countDrop(isErr)
		mu.Unlock()
		return
	}
	if len(queue) == 0 {
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(flushInterval, wake)
	}
	push(item{raw: raw, isErr: isErr})
	due := isErr || len(queue) >= wakeEvents || len(queue)*2 > maxQueueEvents || queueBytes*2 > maxQueueBytes
	mu.Unlock()

	if due {
		wake()
	}
}

// stampRelease adds the release to an error event's context. Only errors
// carry it: that is where it answers which deploy broke something.
func stampRelease(ctx map[string]any) map[string]any {
	mu.Lock()
	r := release
	mu.Unlock()
	if r == "" || ctx["release"] != nil {
		return ctx
	}
	ctx["release"] = r
	return ctx
}

// isError reports whether an event counts as an error for the drop order.
func isError(ctx map[string]any) bool {
	if ctx["error"] != nil || ctx["error_class"] != nil {
		return true
	}
	switch s := ctx["status"].(type) {
	case int:
		return s >= 500
	case int64:
		return s >= 500
	case float64:
		return s >= 500
	}
	return false
}

// trim cuts the event down to at most maxEventBytes of JSON and returns that
// JSON. It copies the context, so the caller's map stays as it was.
func trim(e *event) ([]byte, bool) {
	truncated := false
	if utf8.RuneCountInString(e.Message) > maxMessageChars {
		e.Message = string([]rune(e.Message)[:maxMessageChars])
		truncated = true
	}

	ctx := make(map[string]any, len(e.Context))
	for k, v := range e.Context {
		limit := maxStringBytes
		if k == "stack_trace" {
			limit = maxStackBytes
		}
		if s, ok := v.(string); ok && len(s) > limit {
			v = cutString(s, limit)
			truncated = true
		}
		ctx[k] = v
	}
	if truncated {
		ctx["truncated"] = true
	}
	e.Context = ctx
	if raw, err := json.Marshal(e); err == nil && len(raw) <= maxEventBytes {
		return raw, true
	}

	minimal := map[string]any{"truncated": true}
	for _, k := range minimalContext {
		if v, ok := ctx[k]; ok {
			minimal[k] = v
		}
	}
	e.Context = minimal
	raw, err := json.Marshal(e)
	return raw, err == nil && len(raw) <= maxEventBytes
}

// cutString keeps the first n bytes of s without splitting a character.
func cutString(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// push adds an item at the back. Caller holds mu.
func push(it item) {
	queue = append(queue, it)
	queueBytes += len(it.raw)
	enforceLimits()
}

// putBack returns a batch to the front, in the same order. Caller holds mu.
func putBack(batch []item) {
	queue = slices.Concat(batch, queue)
	for _, it := range batch {
		queueBytes += len(it.raw)
	}
	enforceLimits()
}

// enforceLimits drops events until the queue fits both limits. Caller holds mu.
func enforceLimits() {
	for len(queue) > maxQueueEvents || queueBytes > maxQueueBytes {
		dropOldest()
	}
}

// dropOldest drops the oldest event that is not an error, or the oldest
// error when the queue holds nothing else. Caller holds mu.
func dropOldest() {
	i := 0
	for i < len(queue) && queue[i].isErr {
		i++
	}
	if i == len(queue) {
		i = 0
	}
	it := queue[i]
	copy(queue[1:i+1], queue[:i])
	queue[0] = item{}
	queue = queue[1:]
	queueBytes -= len(it.raw)
	countDrop(it.isErr)
}

// countDrop counts one dropped event. Caller holds mu.
func countDrop(isErr bool) {
	if dropped == 0 {
		fmt.Fprintln(stderr, "lognorth: dropping events; the count is sent once delivery works")
	}
	dropped++
	if isErr {
		droppedErrors++
	}
}

// takeBatch removes up to maxBatchEvents events and maxBatchBytes of JSON
// from the front. Caller holds mu.
func takeBatch() []item {
	size := len(`{"events":[]}`)
	n := 0
	for n < len(queue) && n < batchLimit {
		size += len(queue[n].raw) + 1
		if n > 0 && size > maxBatchBytes {
			break
		}
		n++
	}
	batch := slices.Clone(queue[:n])
	clear(queue[:n])
	queue = queue[n:]
	for _, it := range batch {
		queueBytes -= len(it.raw)
	}
	return batch
}

// wake starts the sender unless it already runs.
func wake() {
	mu.Lock()
	if sending {
		mu.Unlock()
		return
	}
	sending = true
	mu.Unlock()

	go run()
}

// run sends batches until the queue is empty. It waits out any backoff
// before each send.
func run() {
	for {
		mu.Lock()
		if len(queue) == 0 || endpoint == "" {
			sending = false
			mu.Unlock()
			return
		}
		wait := time.Until(retryAt)
		mu.Unlock()

		if wait > 0 {
			time.Sleep(wait)
			continue
		}
		sendNext(context.Background())
	}
}

// Flush sends all buffered events now, one attempt per batch, ignoring any
// backoff. It gives up after 5 seconds. Events it cannot deliver stay
// queued and the sender retries them later.
func Flush() {
	mu.Lock()
	if timer != nil {
		timer.Stop()
		timer = nil
	}
	mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for ctx.Err() == nil && sendNext(ctx) {
	}
	wake()
}

// shutdown flushes within the shutdown deadline and reports the events it
// could not deliver.
func shutdown() {
	done := make(chan struct{})
	go func() {
		Flush()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownTimeout):
	}

	mu.Lock()
	defer mu.Unlock()
	if n := len(queue); n > 0 {
		fmt.Fprintf(stderr, "lognorth: dropped %d events at shutdown\n", n)
	}
}

// sendNext posts the batch at the front of the queue and acts on the answer.
// It returns true when the next batch can go out at once.
func sendNext(ctx context.Context) bool {
	sendMu.Lock()
	defer sendMu.Unlock()

	mu.Lock()
	if len(queue) == 0 || endpoint == "" {
		mu.Unlock()
		return false
	}
	url, key, batch := endpoint, apiKey, takeBatch()
	mu.Unlock()

	status, retryAfter, err := post(ctx, url, key, batch)

	mu.Lock()
	defer mu.Unlock()
	switch {
	case err == nil && status >= 200 && status < 300:
		succeed()
		return true
	case err == nil && (status == 429 || status == 503):
		putBack(batch)
		// The server said when; the backoff stays for failures that do not say.
		if s, err := strconv.Atoi(retryAfter); err == nil {
			retryAt = time.Now().Add(time.Duration(min(max(s, 1), 300)) * retryAfterUnit)
		} else {
			retryAt = time.Now().Add(nextBackoff())
		}
		fail("retry", fmt.Sprintf("lognorth: server busy (HTTP %d), retrying", status))
		return false
	case err == nil && (status == 401 || status == 403 || status == 404):
		putBack(batch)
		retryAt = time.Now().Add(nextConfigWait())
		fail("config", fmt.Sprintf("lognorth: server answered HTTP %d, check the URL and API key; events are kept", status))
		return false
	case err == nil && status >= 400 && status < 500 && status != 408:
		reject(batch)
		return true
	default:
		putBack(batch)
		retryAt = time.Now().Add(jitter(nextBackoff()))
		reason := fmt.Sprintf("HTTP %d", status)
		if err != nil {
			reason = err.Error()
		}
		fail("retry", "lognorth: cannot deliver events ("+reason+"), retrying")
		return false
	}
}

// post sends one batch and returns the status and the Retry-After header.
func post(ctx context.Context, url, key string, batch []item) (int, string, error) {
	var body bytes.Buffer
	body.WriteString(`{"events":[`)
	for i, it := range batch {
		if i > 0 {
			body.WriteByte(',')
		}
		body.Write(it.raw)
	}
	body.WriteString(`]}`)

	req, err := http.NewRequestWithContext(ctx, "POST", url+"/api/v1/events/batch", &body)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, resp.Header.Get("Retry-After"), nil
}

// succeed resets the backoff and queues the drop report. Caller holds mu.
func succeed() {
	backoff, configWait = 0, 0
	batchLimit = min(batchLimit*2, maxBatchEvents)
	if failing != "" {
		fmt.Fprintln(stderr, "lognorth: delivering events again")
		failing = ""
	}
	if dropped == 0 {
		return
	}

	ctx := map[string]any{"dropped": dropped, "dropped_errors": droppedErrors}
	if environment != "" {
		ctx["environment"] = environment
	}
	raw, _ := json.Marshal(event{
		Message:   fmt.Sprintf("LogNorth client dropped %d events", dropped),
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000000Z"),
		Context:   ctx,
	})
	dropped, droppedErrors = 0, 0
	push(item{raw: raw})
}

// reject handles a batch the server can never accept as sent: it splits the
// batch so the next send takes the first half, or drops a single event.
// Caller holds mu.
func reject(batch []item) {
	if len(batch) == 1 {
		countDrop(batch[0].isErr)
		return
	}
	batchLimit = (len(batch) + 1) / 2
	putBack(batch)
}

// fail writes one stderr line when the failure state changes. Caller holds mu.
func fail(state, line string) {
	if failing != state {
		fmt.Fprintln(stderr, line)
		failing = state
	}
}

// nextBackoff returns the current backoff and doubles it. Caller holds mu.
func nextBackoff() time.Duration {
	d := max(backoff, minBackoff)
	backoff = min(d*2, maxBackoff)
	return d
}

// nextConfigWait returns the current wait after a configuration error and
// doubles it. Caller holds mu.
func nextConfigWait() time.Duration {
	d := max(configWait, minConfigWait)
	configWait = min(d*2, maxConfigWait)
	return d
}

// jitter spreads d by up to 20% either way.
func jitter(d time.Duration) time.Duration {
	return d + time.Duration((mrand.Float64()*0.4-0.2)*float64(d))
}
