package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type JobStatus string

const (
	StatusPending   JobStatus = "pending"
	StatusQueued    JobStatus = "queued"
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
	Output      []string `json:"-"`
	subscribers []chan SSEEvent
	cancel      context.CancelFunc
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

const maxOutputLines = 500

func (j *Job) appendLine(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Output = append(j.Output, line)
	if len(j.Output) > maxOutputLines {
		j.Output = append(j.Output[:0], j.Output[len(j.Output)-maxOutputLines:]...)
	}

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
	mu            sync.RWMutex
	jobs          map[string]*Job
	nextID        int
	downloadDir   string
	outputDir     string
	dataDir       string
	maxConcurrent int
	maxRetries    int
	running       int
	queue         []string
	shutdownCtx   context.Context
	shutdownWg    sync.WaitGroup
	saveDebounce  *time.Timer
	saveMu        sync.Mutex
}

func NewDownloadManager(ctx context.Context, dir string, maxConcurrent int, maxRetries int, dataDir string, outputDir string) *DownloadManager {
	m := &DownloadManager{
		jobs:          make(map[string]*Job),
		downloadDir:   dir,
		outputDir:     outputDir,
		dataDir:       dataDir,
		maxConcurrent: maxConcurrent,
		maxRetries:    maxRetries,
		shutdownCtx:   ctx,
	}

	m.loadState()
	m.drainQueue()

	return m
}

func (m *DownloadManager) StartDownload(url string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.shutdownCtx.Err() != nil {
		return nil, fmt.Errorf("server is shutting down")
	}

	for _, j := range m.jobs {
		j.mu.Lock()
		s := j.Status
		j.mu.Unlock()
		if j.URL == url && s != StatusCompleted {
			return nil, fmt.Errorf("a download already exists for this URL")
		}
	}

	m.nextID++
	id := fmt.Sprintf("%d", m.nextID)
	job := &Job{
		ID:         id,
		URL:        url,
		CreatedAt:  time.Now(),
		MaxRetries: m.maxRetries,
	}
	m.jobs[id] = job

	if m.running < m.maxConcurrent {
		job.Status = StatusPending
		m.running++
		m.scheduleSave()
		m.shutdownWg.Add(1)
		go m.runDownload(job)
	} else {
		job.Status = StatusQueued
		m.queue = append(m.queue, id)
		m.scheduleSave()
	}

	return job, nil
}

type BulkResult struct {
	URL   string
	Job   *Job
	Error string
	IsDup bool
}

func (m *DownloadManager) StartBulkDownload(urls []string) []BulkResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Build set of active (non-completed) URLs for O(1) duplicate checking
	activeURLs := make(map[string]bool)
	for _, j := range m.jobs {
		j.mu.Lock()
		s := j.Status
		j.mu.Unlock()
		if s != StatusCompleted {
			activeURLs[j.URL] = true
		}
	}

	shutdownErr := m.shutdownCtx.Err()

	var results []BulkResult
	for _, raw := range urls {
		url := strings.TrimSpace(raw)
		if url == "" {
			continue
		}

		if activeURLs[url] {
			results = append(results, BulkResult{URL: url, IsDup: true})
			continue
		}

		if shutdownErr != nil {
			results = append(results, BulkResult{URL: url, Error: "server is shutting down"})
			continue
		}

		m.nextID++
		id := fmt.Sprintf("%d", m.nextID)
		job := &Job{
			ID:         id,
			URL:        url,
			CreatedAt:  time.Now(),
			MaxRetries: m.maxRetries,
		}
		m.jobs[id] = job

		if m.running < m.maxConcurrent {
			job.Status = StatusPending
			m.running++
			m.shutdownWg.Add(1)
			go m.runDownload(job)
		} else {
			job.Status = StatusQueued
			m.queue = append(m.queue, id)
		}

		activeURLs[url] = true
		results = append(results, BulkResult{URL: url, Job: job})
	}

	m.scheduleSave()
	return results
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
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[j].CreatedAt.Before(jobs[i].CreatedAt)
	})
	return jobs
}

// RetryJob resets a failed job and relaunches download.
func (m *DownloadManager) RetryJob(id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	job, ok := m.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job not found")
	}

	job.mu.Lock()
	if job.Status != StatusFailed {
		job.mu.Unlock()
		return nil, fmt.Errorf("job is not failed")
	}
	job.Error = ""
	job.DoneAt = nil
	job.Progress = 0
	job.RetryCount = 0
	job.Output = nil

	if m.running < m.maxConcurrent {
		job.Status = StatusPending
		m.running++
		job.mu.Unlock()
		m.scheduleSave()
		m.shutdownWg.Add(1)
		go m.runDownload(job)
	} else {
		job.Status = StatusQueued
		m.queue = append(m.queue, id)
		job.mu.Unlock()
		m.scheduleSave()
	}

	return job, nil
}

// DeleteJob removes a single job, cancelling it if running.
func (m *DownloadManager) DeleteJob(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	job, ok := m.jobs[id]
	if !ok {
		return fmt.Errorf("job not found")
	}

	// Remove from queue if queued
	for i, qid := range m.queue {
		if qid == id {
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			break
		}
	}

	// Cancel if running
	job.mu.Lock()
	if job.cancel != nil {
		job.cancel()
	}
	job.mu.Unlock()

	// Clean up job download directory
	os.RemoveAll(filepath.Join(m.downloadDir, id))

	job.closeSubscribers()
	delete(m.jobs, id)
	m.scheduleSave()
	return nil
}

// DeleteAllJobs removes all jobs, cancelling any that are running.
func (m *DownloadManager) DeleteAllJobs() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, job := range m.jobs {
		job.mu.Lock()
		if job.cancel != nil {
			job.cancel()
		}
		job.mu.Unlock()
		job.closeSubscribers()
		os.RemoveAll(filepath.Join(m.downloadDir, job.ID))
	}

	m.jobs = make(map[string]*Job)
	m.queue = nil
	m.running = 0
	m.scheduleSave()
}

// startNextQueued decrements running count and starts the next queued job.
func (m *DownloadManager) startNextQueued() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.running--

	if m.shutdownCtx.Err() != nil {
		return
	}

	for len(m.queue) > 0 {
		id := m.queue[0]
		m.queue = m.queue[1:]
		job, ok := m.jobs[id]
		if !ok {
			continue
		}
		m.running++
		m.shutdownWg.Add(1)
		go m.runDownload(job)
		return
	}
}

// jobExists checks if a job still exists in the manager (not deleted).
func (m *DownloadManager) jobExists(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.jobs[id]
	return ok
}

// runDownload orchestrates download attempts with retry and exponential backoff.
func (m *DownloadManager) runDownload(job *Job) {
	holdsSlot := true
	defer m.shutdownWg.Done()
	defer func() {
		if holdsSlot {
			m.startNextQueued()
		}
	}()

	for {
		if !m.jobExists(job.ID) {
			return
		}

		job.broadcastStatus(StatusRunning)
		m.scheduleSave()

		jobDir := filepath.Join(m.downloadDir, job.ID)
		err := m.executeDownload(job, jobDir)

		if !m.jobExists(job.ID) {
			return
		}

		if err == nil {
			var moveErr error
			if m.outputDir != "" {
				moveErr = m.moveNewFiles(job, jobDir)
			} else {
				moveErr = flattenJobDir(jobDir, m.downloadDir)
			}

			now := time.Now()
			if moveErr != nil {
				log.Printf("move failed for job %s: %v", job.ID, moveErr)
				job.appendLine(fmt.Sprintf("Move failed: %v", moveErr))
				job.mu.Lock()
				job.Status = StatusFailed
				job.Error = fmt.Sprintf("download succeeded but file move failed: %v", moveErr)
				job.DoneAt = &now
				job.mu.Unlock()
			} else {
				job.mu.Lock()
				job.Status = StatusCompleted
				job.DoneAt = &now
				job.Progress = 100
				job.mu.Unlock()
			}
			job.closeSubscribers()
			m.scheduleSave()
			return
		}

		// If shutdown caused the error, leave job in running state for re-queue on restart
		if m.shutdownCtx.Err() != nil {
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
			os.RemoveAll(jobDir)
			m.scheduleSave()
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

		// Release concurrency slot during backoff so other queued jobs can run
		holdsSlot = false
		m.startNextQueued()

		m.scheduleSave()

		job.appendLine(fmt.Sprintf("--- Retry %d/%d in %s ---", attempt, maxRetries, backoff))

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-m.shutdownCtx.Done():
			timer.Stop()
			return
		}

		if !m.jobExists(job.ID) {
			return
		}

		// Re-acquire a concurrency slot before retrying
		m.mu.Lock()
		if m.running < m.maxConcurrent {
			m.running++
			holdsSlot = true
			m.mu.Unlock()
		} else {
			// Re-queue at end; slot will be picked up by startNextQueued
			job.mu.Lock()
			job.Status = StatusQueued
			job.mu.Unlock()
			m.queue = append(m.queue, job.ID)
			m.scheduleSave()
			m.mu.Unlock()
			return
		}
	}
}

// scanCRLF is a bufio.SplitFunc that splits on \r, \n, or \r\n so each
// yt-dlp progress update (written with \r) is emitted immediately.
func scanCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		if data[i] == '\r' {
			if i+1 < len(data) && data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// executeDownload runs the actual subprocess and returns an error if it fails.
func (m *DownloadManager) executeDownload(job *Job, jobDir string) error {
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return fmt.Errorf("failed to create job dir: %v", err)
	}

	ctx, cancel := context.WithCancel(m.shutdownCtx)
	job.mu.Lock()
	job.cancel = cancel
	job.mu.Unlock()

	archivePath := filepath.Join(m.downloadDir, ".ytdlp-archive.txt")
	cmd := exec.CommandContext(ctx, "ytdlp-nfo", "--download-archive", archivePath, job.URL)
	cmd.Dir = jobDir
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("failed to create pipe: %v", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("failed to start: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Split(scanCRLF)
	for scanner.Scan() {
		if trimmed := strings.TrimSpace(scanner.Text()); trimmed != "" {
			job.appendLine(trimmed)
		}
	}

	return cmd.Wait()
}

// moveNewFiles moves completed files from a job's directory to outputDir.
// Uses a staging directory for atomicity so media servers never see partial files.
func (m *DownloadManager) moveNewFiles(job *Job, jobDir string) error {
	if m.outputDir == "" {
		return nil
	}

	if err := os.MkdirAll(m.outputDir, 0755); err != nil {
		return fmt.Errorf("create output dir: %v", err)
	}

	files, err := collectFiles(jobDir)
	if err != nil {
		return fmt.Errorf("scan job dir: %v", err)
	}
	if len(files) == 0 {
		return nil
	}

	// Stage inside outputDir so final rename is same-filesystem and atomic
	stagingDir := filepath.Join(m.outputDir, fmt.Sprintf(".moving-%s", job.ID))
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return fmt.Errorf("create staging dir: %v", err)
	}
	defer os.RemoveAll(stagingDir)

	// Copy files to staging, preserving relative paths
	for _, relPath := range files {
		src := filepath.Join(jobDir, relPath)
		dst := filepath.Join(stagingDir, relPath)

		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return fmt.Errorf("create dir for %s: %v", relPath, err)
		}
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copy %s: %v", relPath, err)
		}
	}

	// Atomically move top-level entries from staging to final location
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return fmt.Errorf("read staging dir: %v", err)
	}
	for _, entry := range entries {
		src := filepath.Join(stagingDir, entry.Name())
		dst := filepath.Join(m.outputDir, entry.Name())
		os.RemoveAll(dst) // remove existing to allow update
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("move %s to output: %v", entry.Name(), err)
		}
	}

	// Remove the entire job directory
	os.RemoveAll(jobDir)

	return nil
}

// collectFiles returns relative paths of all non-hidden files in dir.
func collectFiles(dir string) ([]string, error) {
	var result []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		// Skip hidden files/dirs
		if strings.HasPrefix(filepath.Base(relPath), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		result = append(result, relPath)
		return nil
	})
	return result, err
}

// flattenJobDir moves top-level entries from jobDir to parentDir and removes jobDir.
func flattenJobDir(jobDir, parentDir string) error {
	entries, err := os.ReadDir(jobDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		src := filepath.Join(jobDir, entry.Name())
		dst := filepath.Join(parentDir, entry.Name())
		os.RemoveAll(dst)
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("move %s: %v", entry.Name(), err)
		}
	}
	return os.RemoveAll(jobDir)
}

// copyFile copies a single file from src to dst, preserving permissions.
func copyFile(src, dst string) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()

	info, err := sf.Stat()
	if err != nil {
		return err
	}

	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer df.Close()

	if _, err := io.Copy(df, sf); err != nil {
		return err
	}
	return df.Close()
}

// Shutdown waits for all running downloads to finish and saves final state.
func (m *DownloadManager) Shutdown() {
	m.shutdownWg.Wait()
	m.saveMu.Lock()
	if m.saveDebounce != nil {
		m.saveDebounce.Stop()
		m.saveDebounce = nil
	}
	m.saveMu.Unlock()
	m.executeSave()
}
