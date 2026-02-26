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
		lognorth.Log("homepage visited", map[string]any{"ua": r.UserAgent()})
		fmt.Fprintln(w, "Hello from Go example")
	})

	mux.HandleFunc("GET /error", func(w http.ResponseWriter, r *http.Request) {
		err := fmt.Errorf("something broke")
		lognorth.Error("triggered test error", err, nil)
		http.Error(w, "error triggered, check LogNorth", 500)
	})

	handler := lognorth.Middleware(mux)

	fmt.Println("Go example running on :8080")
	http.ListenAndServe(":8080", handler)
}
