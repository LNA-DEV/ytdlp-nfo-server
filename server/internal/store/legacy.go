package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
)

// V1Job is a job as persisted by the v1 server in jobs.json.
type V1Job struct {
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	DoneAt     *time.Time `json:"doneAt,omitempty"`
	Error      string     `json:"error,omitempty"`
	RetryCount int        `json:"retryCount"`
	Options    struct {
		Format    string `json:"format"`
		AllAudio  bool   `json:"allAudio"`
		Subtitles bool   `json:"subtitles"`
	} `json:"options"`
}

// V1State is the v1 jobs.json document.
type V1State struct {
	NextID int     `json:"nextId"`
	Jobs   []V1Job `json:"jobs"`
}

// ReadV1State parses a v1 jobs.json file.
func ReadV1State(path string) (*V1State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st V1State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &st, nil
}

// ToJob converts a v1 job: completed and failed jobs become history, all
// others are re-queued (§10 step 1).
func (v V1Job) ToJob() (*Job, error) {
	id, err := strconv.ParseInt(v.ID, 10, 64)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("invalid v1 job id %q", v.ID)
	}
	req := config.OptionsRequest{}
	if v.Options.Format != "" {
		f, a, s := v.Options.Format, v.Options.AllAudio, v.Options.Subtitles
		req.Container, req.AllAudio, req.Subtitles = &f, &a, &s
	}
	j := &Job{
		ID:        id,
		URL:       v.URL,
		NormURL:   config.NormalizeURL(v.URL),
		Site:      config.SiteKey(config.Host(v.URL)),
		Requested: req,
		CreatedAt: v.CreatedAt,
		Legacy:    true,
		Attempts:  v.RetryCount,
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now()
	}
	switch v.Status {
	case "completed":
		j.State = JobCompleted
	case "failed":
		j.State = JobFailed
		j.Error = v.Error
		j.ErrorClass = "unknown"
		j.Hint = "Imported from v1. Retry to download it with the new engine."
	default:
		j.State = JobQueued
		j.Attempts = 0
	}
	if j.State.Terminal() {
		if v.DoneAt != nil {
			j.FinishedAt = *v.DoneAt
		} else {
			j.FinishedAt = j.CreatedAt
		}
	}
	return j, nil
}

// ReadArchiveFile reads a yt-dlp download archive ("extractor id" lines).
func ReadArchiveFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseArchive(f)
}

// ParseArchive parses archive lines, skipping blanks and malformed lines.
func ParseArchive(r io.Reader) ([]string, error) {
	var keys []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if ext, id, ok := strings.Cut(line, " "); ok && ext != "" && strings.TrimSpace(id) != "" {
			keys = append(keys, ext+" "+strings.TrimSpace(id))
		}
	}
	return keys, sc.Err()
}
