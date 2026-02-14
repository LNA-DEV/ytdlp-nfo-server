package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
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
)

type Job struct {
	ID        string     `json:"id"`
	URL       string     `json:"url"`
	Status    JobStatus  `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	DoneAt    *time.Time `json:"doneAt,omitempty"`
	Error     string     `json:"error,omitempty"`

	mu          sync.Mutex
	Output      []string     `json:"-"`
	subscribers []chan string
}

// Subscribe returns a snapshot of existing output and a channel for new lines.
func (j *Job) Subscribe() ([]string, chan string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	existing := make([]string, len(j.Output))
	copy(existing, j.Output)
	ch := make(chan string, 128)
	j.subscribers = append(j.subscribers, ch)
	return existing, ch
}

// Unsubscribe removes and closes the given channel.
func (j *Job) Unsubscribe(ch chan string) {
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

func (j *Job) appendLine(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Output = append(j.Output, line)
	for _, ch := range j.subscribers {
		select {
		case ch <- line:
		default:
			// drop if subscriber is slow
		}
	}
}

func (j *Job) closeSubscribers() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, ch := range j.subscribers {
		close(ch)
	}
	j.subscribers = nil
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
		ID:        id,
		URL:       url,
		Status:    StatusPending,
		CreatedAt: time.Now(),
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

func (m *DownloadManager) runDownload(job *Job) {
	job.mu.Lock()
	job.Status = StatusRunning
	job.mu.Unlock()

	if err := os.MkdirAll(m.downloadDir, 0755); err != nil {
		job.mu.Lock()
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("failed to create download dir: %v", err)
		now := time.Now()
		job.DoneAt = &now
		job.mu.Unlock()
		job.closeSubscribers()
		return
	}

	cmd := exec.Command("ytdlp-nfo", job.URL)
	cmd.Dir = m.downloadDir

	// Merge stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		job.mu.Lock()
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("failed to create pipe: %v", err)
		now := time.Now()
		job.DoneAt = &now
		job.mu.Unlock()
		job.closeSubscribers()
		return
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		job.mu.Lock()
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("failed to start: %v", err)
		now := time.Now()
		job.DoneAt = &now
		job.mu.Unlock()
		job.closeSubscribers()
		return
	}

	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			// yt-dlp uses \r for progress updates on the same line
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

	waitErr := cmd.Wait()
	now := time.Now()

	job.mu.Lock()
	job.DoneAt = &now
	if waitErr != nil {
		job.Status = StatusFailed
		job.Error = waitErr.Error()
	} else {
		job.Status = StatusCompleted
	}
	job.mu.Unlock()

	job.closeSubscribers()
}
