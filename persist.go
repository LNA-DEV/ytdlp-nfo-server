package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type persistedJob struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	Status     JobStatus `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
	DoneAt     *time.Time `json:"doneAt,omitempty"`
	Error      string    `json:"error,omitempty"`
	Progress   float64   `json:"progress"`
	RetryCount int       `json:"retryCount"`
	MaxRetries int       `json:"maxRetries"`
	Output     []string  `json:"output,omitempty"`
}

type persistedState struct {
	NextID int            `json:"nextId"`
	Jobs   []persistedJob `json:"jobs"`
}

func jobToPersisted(j *Job) persistedJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	output := make([]string, len(j.Output))
	copy(output, j.Output)
	return persistedJob{
		ID:         j.ID,
		URL:        j.URL,
		Status:     j.Status,
		CreatedAt:  j.CreatedAt,
		DoneAt:     j.DoneAt,
		Error:      j.Error,
		Progress:   j.Progress,
		RetryCount: j.RetryCount,
		MaxRetries: j.MaxRetries,
		Output:     output,
	}
}

func persistedToJob(p persistedJob) *Job {
	return &Job{
		ID:         p.ID,
		URL:        p.URL,
		Status:     p.Status,
		CreatedAt:  p.CreatedAt,
		DoneAt:     p.DoneAt,
		Error:      p.Error,
		Progress:   p.Progress,
		RetryCount: p.RetryCount,
		MaxRetries: p.MaxRetries,
		Output:     p.Output,
	}
}

// saveState writes the current state to jobs.json atomically.
// Must be called with m.mu held (at least RLock).
func (m *DownloadManager) saveState() {
	if m.dataDir == "" {
		return
	}

	state := persistedState{
		NextID: m.nextID,
		Jobs:   make([]persistedJob, 0, len(m.jobs)),
	}
	for _, j := range m.jobs {
		state.Jobs = append(state.Jobs, jobToPersisted(j))
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("persist: failed to marshal state: %v", err)
		return
	}

	tmpPath := filepath.Join(m.dataDir, "jobs.json.tmp")
	finalPath := filepath.Join(m.dataDir, "jobs.json")

	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		log.Printf("persist: failed to write tmp file: %v", err)
		return
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		log.Printf("persist: failed to rename: %v", err)
	}
}

// loadState reads jobs.json and restores jobs into the manager.
// Must be called before the manager starts serving requests.
func (m *DownloadManager) loadState() {
	if m.dataDir == "" {
		return
	}

	data, err := os.ReadFile(filepath.Join(m.dataDir, "jobs.json"))
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("persist: failed to read state: %v", err)
		}
		return
	}

	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("persist: failed to unmarshal state: %v", err)
		return
	}

	m.nextID = state.NextID

	// Separate terminal vs re-queueable jobs
	var requeue []persistedJob
	for _, p := range state.Jobs {
		switch p.Status {
		case StatusCompleted, StatusFailed:
			job := persistedToJob(p)
			m.jobs[job.ID] = job
		default:
			// running, pending, retrying, queued -> re-queue
			p.Status = StatusQueued
			p.Progress = 0
			requeue = append(requeue, p)
		}
	}

	// Sort re-queueable jobs by CreatedAt for FIFO order
	sort.Slice(requeue, func(i, j int) bool {
		return requeue[i].CreatedAt.Before(requeue[j].CreatedAt)
	})
	for _, p := range requeue {
		job := persistedToJob(p)
		m.jobs[job.ID] = job
		m.queue = append(m.queue, job.ID)
	}

	log.Printf("persist: restored %d jobs (%d queued)", len(m.jobs), len(m.queue))
}

// drainQueue starts queued jobs up to the concurrency limit.
// Must be called with m.mu held.
func (m *DownloadManager) drainQueue() {
	for m.running < m.maxConcurrent && len(m.queue) > 0 {
		id := m.queue[0]
		m.queue = m.queue[1:]
		job, ok := m.jobs[id]
		if !ok {
			continue
		}
		m.running++
		m.shutdownWg.Add(1)
		go m.runDownload(job)
	}
}
