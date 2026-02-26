package main

import (
	"fmt"
	"net/http"
	"os"

	lognorth "github.com/karloscodes/lognorth-sdk-go"
)

func main() {
	lognorth.Config("http://localhost:8080", os.Getenv("LOGNORTH_API_KEY"))

	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello from Go example")
	})

	mux.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {
		lognorth.LogCtx(r.Context(), "listing users", map[string]any{"page": 1, "limit": 20})
		fmt.Fprintln(w, `[{"id":1,"name":"Alice"},{"id":2,"name":"Bob"}]`)
	})

	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		lognorth.LogCtx(r.Context(), "order created", map[string]any{"order_id": 42, "total": 99.95})
		lognorth.LogCtx(r.Context(), "sending confirmation email", map[string]any{"order_id": 42})
		w.WriteHeader(201)
		fmt.Fprintln(w, `{"id":42}`)
	})

	mux.HandleFunc("GET /error", func(w http.ResponseWriter, r *http.Request) {
		err := fmt.Errorf("something broke")
		lognorth.ErrorCtx(r.Context(), "triggered test error", err, nil)
		http.Error(w, "error triggered, check LogNorth", 500)
	})

	mux.HandleFunc("GET /timeout", func(w http.ResponseWriter, r *http.Request) {
		lognorth.LogCtx(r.Context(), "starting slow operation", nil)
		err := fmt.Errorf("connection timeout after 30s")
		lognorth.ErrorCtx(r.Context(), "database query failed", err, map[string]any{"query": "SELECT * FROM users"})
		http.Error(w, "timeout error triggered", 500)
	})

	handler := lognorth.Middleware(mux)

	fmt.Println("Go example running on :8082")
	http.ListenAndServe(":8082", handler)
}
