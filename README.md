# lognorth-sdk-go

Official Go SDK for [LogNorth](https://lognorth.com) - self-hosted error tracking.

## Install

```bash
go get github.com/karloscodes/lognorth-sdk-go
```

## Use

```go
package main

import lognorth "github.com/karloscodes/lognorth-sdk-go"

func main() {
	lognorth.Config("https://logs.yoursite.com", "your-api-key")

	lognorth.Log("User signed up", map[string]any{"user_id": 123})
	lognorth.Error("Checkout failed", err, map[string]any{"order_id": 42})
}
```

## With slog

```go
slog.SetDefault(slog.New(lognorth.NewHandler()))

slog.Info("User signed up", "user_id", 123)
slog.Error("Checkout failed", "error", err)
```

## Middleware

```go
package main

import (
	"net/http"
	lognorth "github.com/karloscodes/lognorth-sdk-go"
)

func main() {
	lognorth.Config("https://logs.yoursite.com", "your-api-key")

	http.HandleFunc("/", homeHandler)
	http.HandleFunc("/users", usersHandler)

	// Wrap with middleware
	http.ListenAndServe(":8080", lognorth.Middleware(http.DefaultServeMux))
}
```

### Who hit the error, and in which release

Name the signed-in user inside a handler. Use an ID, not an email:

```go
lognorth.SetUser(r.Context(), strconv.Itoa(user.ID))
```

The request event carries it, and so do errors you log with the request's context. LogNorth then shows how many users an issue hit.

Errors also carry the release. The SDK reads `LOGNORTH_RELEASE`, `GIT_SHA`, `KAMAL_VERSION`, or the commit variable of Render, Heroku, Railway, Vercel, or Coolify. Or set it:

```go
lognorth.Configure(lognorth.Options{URL: url, APIKey: key, Release: version})
```

When the release is set, the SDK logs `Release <version> started` once at startup. LogNorth marks each release's first start on its charts.

A failed request (5xx) also carries its user agent, so you can tell a bot from a browser.

### Skipping noisy endpoints

Health checks and uptime probes swamp the log feed if you let them
through. Tell the SDK which paths to skip:

```go
lognorth.IgnorePaths("/healthz", "/ping", "/up")
```

No default list — opt in to the ones that match your deployment.
Matches exact path or `path/…` prefix.

## How It Works

- `Log()` and `Error()` return at once. They never block your app.
- Events wait in a memory queue. The SDK sends them when 10 are queued, after 5 seconds, or at once for an error.
- One request is in flight at a time. A batch holds at most 500 events and 1 MB.
- Events stay in order, also across retries.

### When delivery fails

- Network errors, timeouts, and 5xx answers: the SDK keeps the events and retries. It waits 1 second, then doubles the wait up to 60 seconds.
- 429 and 503: the SDK waits as long as `Retry-After` says.
- 401, 403, and 404: the SDK keeps the events, writes one line to stderr, and retries after 60 seconds, up to every 5 minutes. Fix the URL or the API key and the events arrive.
- 400, 413, and other 4xx: the SDK splits the batch in two and sends the halves. It drops a single event only when the server still rejects it.

### Memory limits

- The queue holds at most 10,000 events or 10 MB.
- Each event is cut to 64 KB. Long strings are shortened and the event gets `truncated: true`.
- When the queue is full, the SDK drops the oldest log events first. It keeps errors longest.
- After the next success, the SDK sends one event, `LogNorth client dropped N events`, with the counts in `dropped` and `dropped_errors`.

### Shutdown

On SIGINT and SIGTERM the SDK sends what is queued, within 5 seconds. Call `lognorth.Flush()` before your app exits in other ways.

## License

MIT
