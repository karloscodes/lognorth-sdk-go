# lognorth-sdk-go

Official Go SDK for [LogNorth](https://lognorth.com) - self-hosted error tracking.

Uses Go's standard `log/slog` package.

## Install

```bash
go get github.com/karloscodes/lognorth-sdk-go
```

## Use

```bash
export LOGNORTH_API_KEY=your_key
export LOGNORTH_URL=https://logs.yoursite.com
```

```go
package main

import (
	"log/slog"

	lognorth "github.com/karloscodes/lognorth-sdk-go"
)

func main() {
	slog.SetDefault(slog.New(lognorth.NewHandler()))

	slog.Info("User signed up", "user_id", 123)
	slog.Error("Checkout failed", "error", err)
}
```

That's it. Batches automatically, errors sent immediately, flushes on shutdown.

## Middleware

```go
mux := http.NewServeMux()
http.ListenAndServe(":8080", lognorth.Middleware(mux))
```

## Config (optional)

```go
lognorth.Config("api-key", "https://logs.yoursite.com")
```

## License

MIT
