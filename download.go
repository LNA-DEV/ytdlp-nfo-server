package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type JobStatus string

const (
	StatusPending   JobStatus = "pending"
	StatusRunning   JobStatus = "running"
	StatusCompleted JobStatus = "completed"
	StatusFailed    JobStatus = "failed"
	StatusRetrying  JobStatus = "retrying"
)

type SSEEvent struct {
	Type string // "message", "progress", "status", "done"
	Data string
}

type Job struct {
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	Status     JobStatus  `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	DoneAt     *time.Time `json:"doneAt,omitempty"`
	Error      string     `json:"error,omitempty"`
	Progress   float64    `json:"progress"`
	RetryCount int        `json:"retryCount"`
	MaxRetries int        `json:"maxRetries"`

	mu          sync.Mutex
	Output      []string     `json:"-"`
	subscribers []chan SSEEvent
}

var progressRegex = regexp.MustCompile(`\[download\]\s+([\d.]+)%`)

// Subscribe returns a snapshot of existing output and a channel for new events.
func (j *Job) Subscribe() ([]string, chan SSEEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	existing := make([]string, len(j.Output))
	copy(existing, j.Output)
	ch := make(chan SSEEvent, 128)
	j.subscribers = append(j.subscribers, ch)
	return existing, ch
}

// Unsubscribe removes and closes the given channel.
func (j *Job) Unsubscribe(ch chan SSEEvent) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i, sub := range j.subscribers {
		if sub == ch {
			j.subscribers = append(j.subscribers[:i], j.subscribers[i+1:]...)
			close(ch)
			return
		}
	}
}

func (j *Job) broadcast(evt SSEEvent) {
	for _, ch := range j.subscribers {
		select {
		case ch <- evt:
		default:
		}
	}
}

func (j *Job) appendLine(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Output = append(j.Output, line)

	// Check for progress
	if m := progressRegex.FindStringSubmatch(line); m != nil {
		if pct, err := strconv.ParseFloat(m[1], 64); err == nil {
			j.Progress = pct
			j.broadcast(SSEEvent{Type: "progress", Data: m[1]})
		}
	}

	j.broadcast(SSEEvent{Type: "message", Data: line})
}

func (j *Job) closeSubscribers() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, ch := range j.subscribers {
		close(ch)
	}
	j.subscribers = nil
}

func (j *Job) broadcastStatus(s JobStatus) {
	j.mu.Lock()
	j.Status = s
	j.broadcast(SSEEvent{Type: "status", Data: string(s)})
	j.mu.Unlock()
}

type DownloadManager struct {
	mu          sync.RWMutex
	jobs        map[string]*Job
	nextID      int
	downloadDir string
}

func NewDownloadManager(dir string) *DownloadManager {
	return &DownloadManager{
		jobs:        make(map[string]*Job),
		downloadDir: dir,
	}
}

func (m *DownloadManager) StartDownload(url string) *Job {
	m.mu.Lock()
	m.nextID++
	id := fmt.Sprintf("%d", m.nextID)
	job := &Job{
		ID:         id,
		URL:        url,
		Status:     StatusPending,
		CreatedAt:  time.Now(),
		MaxRetries: 3,
	}
	m.jobs[id] = job
	m.mu.Unlock()

	go m.runDownload(job)
	return job
}

func (m *DownloadManager) GetJob(id string) (*Job, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.jobs[id]
	return job, ok
}

func (m *DownloadManager) ListJobs() []*Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	// Sort newest first
	for i := 0; i < len(jobs); i++ {
		for j := i + 1; j < len(jobs); j++ {
			if jobs[j].CreatedAt.After(jobs[i].CreatedAt) {
				jobs[i], jobs[j] = jobs[j], jobs[i]
			}
		}
	}
	return jobs
}

// RetryJob resets a failed job and relaunches download.
func (m *DownloadManager) RetryJob(id string) (*Job, error) {
	m.mu.RLock()
	job, ok := m.jobs[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("job not found")
	}

	job.mu.Lock()
	if job.Status != StatusFailed {
		job.mu.Unlock()
		return nil, fmt.Errorf("job is not failed")
	}
	job.Status = StatusPending
	job.Error = ""
	job.DoneAt = nil
	job.Progress = 0
	job.RetryCount = 0
	job.Output = nil
	job.mu.Unlock()

	go m.runDownload(job)
	return job, nil
}

// runDownload orchestrates download attempts with retry and exponential backoff.
func (m *DownloadManager) runDownload(job *Job) {
	for {
		job.broadcastStatus(StatusRunning)

		err := m.executeDownload(job)
		if err == nil {
			now := time.Now()
			job.mu.Lock()
			job.Status = StatusCompleted
			job.DoneAt = &now
			job.Progress = 100
			job.mu.Unlock()
			job.closeSubscribers()
			return
		}

		job.mu.Lock()
		job.RetryCount++
		attempt := job.RetryCount
		maxRetries := job.MaxRetries
		job.mu.Unlock()

		if attempt >= maxRetries {
			now := time.Now()
			job.mu.Lock()
			job.Status = StatusFailed
			job.Error = err.Error()
			job.DoneAt = &now
			job.mu.Unlock()
			job.closeSubscribers()
			return
		}

		// Exponential backoff: 10s * 3^(attempt-1) => 10s, 30s, 90s
		backoff := 10 * time.Second
		for i := 1; i < attempt; i++ {
			backoff *= 3
		}

		job.mu.Lock()
		job.Status = StatusRetrying
		job.Progress = 0
		job.broadcast(SSEEvent{Type: "status", Data: string(StatusRetrying)})
		job.mu.Unlock()

		job.appendLine(fmt.Sprintf("--- Retry %d/%d in %s ---", attempt, maxRetries, backoff))
		time.Sleep(backoff)
	}
}

// executeDownload runs the actual subprocess and returns an error if it fails.
func (m *DownloadManager) executeDownload(job *Job) error {
	if err := os.MkdirAll(m.downloadDir, 0755); err != nil {
		return fmt.Errorf("failed to create download dir: %v", err)
	}

	cmd := exec.Command("ytdlp-nfo", job.URL)
	cmd.Dir = m.downloadDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create pipe: %v", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start: %v", err)
	}

	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			parts := strings.Split(strings.TrimRight(line, "\r\n"), "\r")
			for _, part := range parts {
				trimmed := strings.TrimSpace(part)
				if trimmed != "" {
					job.appendLine(trimmed)
				}
			}
		}
		if err != nil {
			break
		}
	}

	return cmd.Wait()
}
