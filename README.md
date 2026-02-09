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

## How It Works

- `Log()` batches events (10 or 5s)
- `Error()` sends immediately
- Auto-flushes on shutdown

## License

MIT
