package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
)

//go:embed static
var staticFiles embed.FS

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	port := getEnv("PORT", "8080")
	downloadDir := getEnv("DOWNLOAD_DIR", "./downloads")
	dataDir := getEnv("DATA_DIR", "")

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

	mgr := NewDownloadManager(downloadDir, maxConcurrent, maxRetries, dataDir)

	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/download", handleSubmit(mgr))
	mux.HandleFunc("GET /api/jobs", handleListJobs(mgr))
	mux.HandleFunc("GET /api/jobs/{id}", handleJobStatus(mgr))
	mux.HandleFunc("GET /api/jobs/{id}/stream", handleJobStream(mgr))
	mux.HandleFunc("POST /api/jobs/{id}/retry", handleRetryJob(mgr))
	mux.HandleFunc("DELETE /api/jobs/{id}", handleDeleteJob(mgr))
	mux.HandleFunc("DELETE /api/jobs", handleDeleteAllJobs(mgr))

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("failed to create static sub-filesystem: %v", err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(staticSub)))

	log.Printf("Starting server on :%s (downloads -> %s, maxConcurrent: %d)", port, downloadDir, maxConcurrent)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
