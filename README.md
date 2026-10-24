# lognorth-sdk-go

[![CI](https://github.com/karloscodes/lognorth-sdk-go/actions/workflows/ci.yml/badge.svg)](https://github.com/karloscodes/lognorth-sdk-go/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/karloscodes/lognorth-sdk-go.svg)](https://pkg.go.dev/github.com/karloscodes/lognorth-sdk-go) [![Release](https://img.shields.io/github/v/release/karloscodes/lognorth-sdk-go)](https://github.com/karloscodes/lognorth-sdk-go/releases) [![License](https://img.shields.io/github/license/karloscodes/lognorth-sdk-go)](LICENSE)

Go client for LogNorth, a self-hosted error tracker. It sends logs and errors from your app to your LogNorth server.

[LogNorth](https://lognorth.com) is self-hosted. The [Go SDK docs](https://lognorth.com/docs/integrations/go/) cover setup on the server side.

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

Call `Config` once at startup. `Log` sends a message. `Error` sends an error and starts sending at once.

## With slog

Use the standard `log/slog` package with the LogNorth handler:

```go
slog.SetDefault(slog.New(lognorth.NewHandler()))

slog.Info("User signed up", "user_id", 123)
slog.Error("Checkout failed", "error", err)
```

Pass the request context to `slog.InfoContext` or `slog.ErrorContext`. The handler then adds the request's trace ID to the event.

## Middleware

`lognorth.Middleware` wraps an `http.Handler` and sends one event per request:

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

	http.ListenAndServe(":8080", lognorth.Middleware(http.DefaultServeMux))
}
```

The middleware reads the `X-Trace-ID` request header. If the header is empty, it makes a new trace ID. It sets the same ID in the response header. On Go 1.22 and later, the event's `route` is the `ServeMux` pattern. For other routers, call `lognorth.WithRoute` to set the route.

### Who hit the error, and in which release

Name the signed-in user inside a handler or an auth middleware that runs inside `lognorth.Middleware`. Use an ID, not an email:

```go
lognorth.SetUser(r.Context(), strconv.Itoa(user.ID))
```

The request event carries the user. So does an error that you log through slog with the request's context.

Errors also carry the release. When you do not set one, the SDK reads the first set variable in this order: `LOGNORTH_RELEASE`, `GIT_SHA`, `GIT_COMMIT`, `SOURCE_COMMIT`, `KAMAL_VERSION`, `RENDER_GIT_COMMIT`, `HEROKU_SLUG_COMMIT`, `SOURCE_VERSION`, `RAILWAY_GIT_COMMIT_SHA`, `VERCEL_GIT_COMMIT_SHA`. To set it yourself, use `Configure` (see Configuration).

When the release is set, `Config` or `Configure` sends `Release <version> started` once per release.

A failed request (status 500 or higher) also carries its user agent. You can tell a bot from a browser this way.

### Skipping noisy endpoints

Health checks and probes send many requests. Tell the middleware which paths to skip:

```go
lognorth.IgnorePaths("/healthz", "/ping", "/up")
```

There is no default list. A path matches when it equals the ignored path or starts with that path followed by `/`.

## Configuration

`Config(url, key)` sets the endpoint and API key. `Configure` sets all options in one call:

```go
lognorth.Configure(lognorth.Options{
	URL:         "https://logs.yoursite.com",
	APIKey:      "your-api-key",
	Environment: "production",
	Release:     version,
})
```

| Field | Meaning |
|---|---|
| `URL` | Your LogNorth server URL. |
| `APIKey` | The API key for the app. |
| `Environment` | A label on every event, such as `production` or `staging`. Optional. |
| `Release` | The version that runs, such as a Git SHA. Optional. When empty, the SDK reads the variables listed above. |
| `Enabled` | A `*bool`. When nil, the SDK sends events unless `Environment` is `development` or `test`. Set it to override. |

To change settings later, call `SetEnvironment(env)` or `SetEnabled(bool)`. `SetEnabled(false)` makes `Log`, `Error`, and the handler do nothing.

## How it works

- `Log` and `Error` return at once. They never block your app.
- Events wait in memory. The SDK sends them when 10 are queued, after 5 seconds, or at once for an error.
- One request is in flight at a time. A batch holds at most 500 events or 1 MB.
- Events keep their order, also across retries.

### When delivery fails

- Network errors, timeouts, and 5xx answers: the SDK keeps the events and retries. The wait starts at 1 second and doubles up to 60 seconds, with jitter.
- 429 and 503: the SDK waits as long as the `Retry-After` header says, clamped to 1 to 300 seconds. Without the header, it uses the normal backoff.
- 401, 403, and 404: the SDK keeps the events and writes one line to stderr. It retries after 60 seconds, doubling the wait up to 5 minutes. Fix the URL or the API key, and the events arrive.
- Other 4xx answers, such as 400: the SDK halves the batch and sends each half. It drops one event only when the server still rejects it alone.

### Limits

- The queue holds at most 10,000 events or 10 MB. When it is full, the SDK drops the oldest log event first. It keeps errors longest.
- Each event is at most 64 KB. Long text is cut and the event gets `truncated: true`.
- After delivery works again, the SDK sends one event, `LogNorth client dropped N events`. It counts the drops in `dropped` and `dropped_errors`.

### Shutdown

On SIGINT and SIGTERM, the SDK sends what is queued. It waits at most 5 seconds, then exits. Call `lognorth.Flush()` before your app exits by another path. `Flush` also waits at most 5 seconds.

## Development

```bash
go vet ./...
go test -race ./...
```

## License

MIT. See [LICENSE](LICENSE).
