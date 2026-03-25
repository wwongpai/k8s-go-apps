package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /hello", helloHandler)
	mux.HandleFunc("GET /fibonacci/{n}", fibonacciHandler)
	mux.HandleFunc("GET /work/{ms}", workHandler)
	mux.HandleFunc("POST /echo", echoHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}

	log.Printf(`{"service":"orchestrion-demo","message":"starting on :%s"}`, port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "orchestrion-demo"})
}

func helloHandler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "world"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": fmt.Sprintf("Hello, %s!", name),
		"time":    time.Now().Format(time.RFC3339),
	})
}

func fibonacciHandler(w http.ResponseWriter, r *http.Request) {
	nStr := r.PathValue("n")
	n, err := strconv.Atoi(nStr)
	if err != nil || n < 0 || n > 90 {
		http.Error(w, `{"error":"n must be 0-90"}`, http.StatusBadRequest)
		return
	}
	result := fibonacci(n)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"n":      n,
		"result": result,
	})
}

func workHandler(w http.ResponseWriter, r *http.Request) {
	msStr := r.PathValue("ms")
	ms, err := strconv.Atoi(msStr)
	if err != nil || ms < 0 || ms > 5000 {
		http.Error(w, `{"error":"ms must be 0-5000"}`, http.StatusBadRequest)
		return
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"slept_ms": ms,
	})
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	var body any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"echo": body,
	})
}

func fibonacci(n int) int64 {
	if n <= 1 {
		return int64(n)
	}
	var a, b int64 = 0, 1
	for i := 2; i <= n; i++ {
		a, b = b, a+b
	}
	return b
}
