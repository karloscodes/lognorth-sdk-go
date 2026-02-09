# lognorth-sdk-go

Official Go SDK for [LogNorth](https://lognorth.com) - self-hosted error tracking and logging.

Uses Go's standard `log/slog` package.

## Install

```bash
go get github.com/karloscodes/lognorth-sdk-go
```

## Quick Start

```bash
export LOGNORTH_API_KEY=your_api_key
export LOGNORTH_URL=https://logs.yoursite.com
```

```go
package main

import (
	"log/slog"
	"os"

	lognorth "github.com/karloscodes/lognorth-sdk-go"
)

func main() {
	handler := lognorth.NewHandler(lognorth.Config{
		APIKey:   os.Getenv("LOGNORTH_API_KEY"),
		Endpoint: os.Getenv("LOGNORTH_URL"),
	})
	slog.SetDefault(slog.New(handler))

	slog.Info("User signed up", "user_id", 123)
	slog.Error("Checkout failed", "error", err, "order_id", order.ID)

	// No need to call Flush - auto-flushes on SIGINT/SIGTERM
}
```

## HTTP Middleware

```go
import (
	"net/http"

	lognorth "github.com/karloscodes/lognorth-sdk-go"
)

func main() {
	handler := lognorth.NewHandler(lognorth.Config{
		APIKey:   os.Getenv("LOGNORTH_API_KEY"),
		Endpoint: os.Getenv("LOGNORTH_URL"),
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/", homeHandler)

	// Logs all requests: "GET /path → 200"
	http.ListenAndServe(":8080", lognorth.Middleware(handler)(mux))
}
```

Works with Chi, gorilla/mux, and any router that uses `http.Handler`.

## How It Works

| Feature | Behavior |
|---------|----------|
| Regular logs | Batched (10 events or 5s), 1 retry |
| Errors (`slog.Error`) | Sent immediately, 3 retries |
| Buffer limit | 1000 events, drops oldest logs first |
| Backpressure | Exponential backoff on 429/503 |
| Shutdown | Auto-flush on SIGINT/SIGTERM |

## Configuration

```go
handler := lognorth.NewHandler(lognorth.Config{
	APIKey:        "your-api-key",      // Required
	Endpoint:      "https://logs.example.com",
	BatchSize:     10,                  // Events before auto-flush
	FlushInterval: 5 * time.Second,     // Time before auto-flush
	MaxBufferSize: 1000,                // Max buffered events
})
```

## With Context

```go
log := slog.New(handler).With("service", "api", "version", "1.0")

log.Info("Request handled", "path", "/users", "status", 200)
```

## Manual Flush

```go
handler.Flush() // If you need to flush manually
```

## License

MIT
