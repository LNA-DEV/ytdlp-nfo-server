package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed static
var staticFiles embed.FS

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func authMiddleware(next http.Handler, password string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if password == "" || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		token := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		}
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		if subtle.ConstantTimeCompare([]byte(token), []byte(password)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}

		next.ServeHTTP(w, r)
	})
}

func main() {
	port := getEnv("PORT", "8080")
	downloadDir := getEnv("DOWNLOAD_DIR", "./downloads")
	dataDir := getEnv("DATA_DIR", "")
	password := getEnv("PASSWORD", "")

	maxConcurrent := 3
	if v := os.Getenv("MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxConcurrent = n
		}
	}

	maxRetries := 3
	if v := os.Getenv("MAX_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxRetries = n
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := NewDownloadManager(ctx, downloadDir, maxConcurrent, maxRetries, dataDir)

	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/download", handleSubmit(mgr))
	mux.HandleFunc("POST /api/download/bulk", handleBulkSubmit(mgr))
	mux.HandleFunc("GET /api/jobs", handleListJobs(mgr))
	mux.HandleFunc("GET /api/jobs/{id}", handleJobStatus(mgr))
	mux.HandleFunc("GET /api/jobs/{id}/stream", handleJobStream(mgr))
	mux.HandleFunc("POST /api/jobs/{id}/retry", handleRetryJob(mgr))
	mux.HandleFunc("DELETE /api/jobs/{id}", handleDeleteJob(mgr))
	mux.HandleFunc("DELETE /api/jobs", handleDeleteAllJobs(mgr))
	mux.HandleFunc("GET /api/auth", handleAuth())

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("failed to create static sub-filesystem: %v", err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(staticSub)))

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: authMiddleware(mux, password),
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Printf("Starting server on :%s (downloads -> %s, maxConcurrent: %d)", port, downloadDir, maxConcurrent)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-sigCh
	log.Println("Shutdown signal received")

	// 1. Stop accepting new HTTP requests, drain in-flight (5s max)
	httpCtx, httpCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer httpCancel()
	srv.Shutdown(httpCtx)

	// 2. Cancel all downloads and backoff sleeps
	cancel()

	// 3. Wait for goroutines to finish, then save final state
	mgr.Shutdown()

	log.Println("Shutdown complete")
}
