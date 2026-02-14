package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type downloadRequest struct {
	URL string `json:"url"`
}

type jobSummary struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	Status     JobStatus `json:"status"`
	CreatedAt  string    `json:"createdAt"`
	DoneAt     string    `json:"doneAt,omitempty"`
	Error      string    `json:"error,omitempty"`
	Progress   float64   `json:"progress"`
	RetryCount int       `json:"retryCount"`
	MaxRetries int       `json:"maxRetries"`
}

type jobDetail struct {
	jobSummary
	Output []string `json:"output"`
}

func toSummary(j *Job) jobSummary {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := jobSummary{
		ID:         j.ID,
		URL:        j.URL,
		Status:     j.Status,
		CreatedAt:  j.CreatedAt.Format("2006-01-02T15:04:05Z"),
		Error:      j.Error,
		Progress:   j.Progress,
		RetryCount: j.RetryCount,
		MaxRetries: j.MaxRetries,
	}
	if j.DoneAt != nil {
		s.DoneAt = j.DoneAt.Format("2006-01-02T15:04:05Z")
	}
	return s
}

func toDetail(j *Job) jobDetail {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := jobSummary{
		ID:         j.ID,
		URL:        j.URL,
		Status:     j.Status,
		CreatedAt:  j.CreatedAt.Format("2006-01-02T15:04:05Z"),
		Error:      j.Error,
		Progress:   j.Progress,
		RetryCount: j.RetryCount,
		MaxRetries: j.MaxRetries,
	}
	if j.DoneAt != nil {
		s.DoneAt = j.DoneAt.Format("2006-01-02T15:04:05Z")
	}
	output := make([]string, len(j.Output))
	copy(output, j.Output)
	return jobDetail{jobSummary: s, Output: output}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func handleSubmit(mgr *DownloadManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req downloadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		req.URL = strings.TrimSpace(req.URL)
		if req.URL == "" {
			http.Error(w, `{"error":"url is required"}`, http.StatusBadRequest)
			return
		}

		job := mgr.StartDownload(req.URL)
		writeJSON(w, http.StatusCreated, toSummary(job))
	}
}

func handleListJobs(mgr *DownloadManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jobs := mgr.ListJobs()
		summaries := make([]jobSummary, len(jobs))
		for i, j := range jobs {
			summaries[i] = toSummary(j)
		}
		writeJSON(w, http.StatusOK, summaries)
	}
}

func handleJobStatus(mgr *DownloadManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		job, ok := mgr.GetJob(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, toDetail(job))
	}
}

func handleJobStream(mgr *DownloadManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		job, ok := mgr.GetJob(id)
		if !ok {
			http.NotFound(w, r)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		existing, ch := job.Subscribe()
		defer job.Unsubscribe(ch)

		// Send existing output
		for _, line := range existing {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}

		// Send current progress if any
		job.mu.Lock()
		isDone := job.Status == StatusCompleted || job.Status == StatusFailed
		status := job.Status
		progress := job.Progress
		job.mu.Unlock()

		if progress > 0 {
			fmt.Fprintf(w, "event: progress\ndata: %.1f\n\n", progress)
		}

		if isDone {
			fmt.Fprintf(w, "event: done\ndata: %s\n\n", status)
			flusher.Flush()
			return
		}
		flusher.Flush()

		// Stream new events
		for {
			select {
			case evt, open := <-ch:
				if !open {
					job.mu.Lock()
					status = job.Status
					job.mu.Unlock()
					fmt.Fprintf(w, "event: done\ndata: %s\n\n", status)
					flusher.Flush()
					return
				}
				switch evt.Type {
				case "message":
					fmt.Fprintf(w, "data: %s\n\n", evt.Data)
				default:
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, evt.Data)
				}
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
}

func handleRetryJob(mgr *DownloadManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		job, err := mgr.RetryJob(id)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, toSummary(job))
	}
}
