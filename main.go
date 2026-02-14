package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
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

	mgr := NewDownloadManager(downloadDir)

	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/download", handleSubmit(mgr))
	mux.HandleFunc("GET /api/jobs", handleListJobs(mgr))
	mux.HandleFunc("GET /api/jobs/{id}", handleJobStatus(mgr))
	mux.HandleFunc("GET /api/jobs/{id}/stream", handleJobStream(mgr))

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("failed to create static sub-filesystem: %v", err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(staticSub)))

	log.Printf("Starting server on :%s (downloads -> %s)", port, downloadDir)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
