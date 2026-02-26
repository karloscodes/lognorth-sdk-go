package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"

	lognorth "github.com/karloscodes/lognorth-sdk-go"
)

func main() {
	lognorth.Config("http://localhost:8080", os.Getenv("LOGNORTH_API_KEY"))
	slog.SetDefault(slog.New(lognorth.NewHandler()))

	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello from Go example")
	})

	mux.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {
		log := lognorth.From(r.Context())
		log.Info("listing users", "page", 1, "limit", 20)
		fmt.Fprintln(w, `[{"id":1,"name":"Alice"},{"id":2,"name":"Bob"}]`)
	})

	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		log := lognorth.From(r.Context())
		log.Info("order created", "order_id", 42, "total", 99.95)
		log.Info("sending confirmation email", "order_id", 42)
		w.WriteHeader(201)
		fmt.Fprintln(w, `{"id":42}`)
	})

	mux.HandleFunc("GET /error", func(w http.ResponseWriter, r *http.Request) {
		log := lognorth.From(r.Context())
		err := fmt.Errorf("something broke")
		log.Error("triggered test error", "error", err)
		http.Error(w, "error triggered, check LogNorth", 500)
	})

	mux.HandleFunc("GET /checkout", func(w http.ResponseWriter, r *http.Request) {
		log := lognorth.From(r.Context())
		log.Info("checkout started", "user_id", 7, "cart_items", 3)
		log.Info("payment processed", "amount", 149.99, "currency", "USD")
		log.Info("inventory updated", "sku", "WIDGET-42", "remaining", 18)
		log.Info("confirmation email sent", "to", "alice@example.com")
		w.WriteHeader(201)
		fmt.Fprintln(w, `{"order_id":99}`)
	})

	mux.HandleFunc("GET /failing", func(w http.ResponseWriter, r *http.Request) {
		log := lognorth.From(r.Context())
		log.Info("starting data sync")
		log.Error("connection refused", "error", fmt.Errorf("dial tcp 10.0.0.5:5432: connection refused"), "host", "db-replica-2")
		log.Info("retrying with primary")
		log.Error("query timeout", "error", fmt.Errorf("context deadline exceeded after 30s"), "query", "SELECT * FROM events")
		http.Error(w, "sync failed", 500)
	})

	handler := lognorth.Middleware(mux)

	fmt.Println("Go example running on :8082")
	http.ListenAndServe(":8082", handler)
}
