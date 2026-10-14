package ytdlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/procutil"
)

// Progress is one parsed progress-template update.
type Progress struct {
	Status          string  `json:"status"`
	DownloadedBytes float64 `json:"downloaded_bytes"`
	TotalBytes      float64 `json:"total_bytes"`
	TotalEstimate   float64 `json:"total_bytes_estimate"`
	Speed           float64 `json:"speed"`
	ETA             float64 `json:"eta"`
	FragmentIndex   float64 `json:"fragment_index"`
	FragmentCount   float64 `json:"fragment_count"`
	Filename        string  `json:"filename"`
}

// Percent returns the completion of the current file (0-100).
func (p Progress) Percent() float64 {
	total := p.TotalBytes
	if total <= 0 {
		total = p.TotalEstimate
	}
	if total > 0 {
		return min(100, p.DownloadedBytes/total*100)
	}
	if p.FragmentCount > 0 {
		return min(100, p.FragmentIndex/p.FragmentCount*100)
	}
	if p.Status == "finished" {
		return 100
	}
	return 0
}

// ParseProgress parses a progress line ("@@P {...}").
func ParseProgress(line string) (Progress, bool) {
	raw, ok := strings.CutPrefix(line, ProgressPrefix)
	if !ok {
		return Progress{}, false
	}
	var p Progress
	// Unknown values are "NA" strings in some versions; decode leniently.
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return Progress{}, false
	}
	f := func(k string) float64 {
		if v, ok := m[k].(float64); ok {
			return v
		}
		return 0
	}
	p.Status, _ = m["status"].(string)
	p.Filename, _ = m["filename"].(string)
	p.DownloadedBytes = f("downloaded_bytes")
	p.TotalBytes = f("total_bytes")
	p.TotalEstimate = f("total_bytes_estimate")
	p.Speed = f("speed")
	p.ETA = f("eta")
	p.FragmentIndex = f("fragment_index")
	p.FragmentCount = f("fragment_count")
	return p, true
}

// Call is one yt-dlp invocation.
type Call struct {
	Bin  string
	Args []string
	Dir  string
	Env  []string
	// CaptureStdout collects stdout (the -J JSON) instead of treating it as
	// log lines.
	CaptureStdout bool
	// OnLine receives every non-progress output line.
	OnLine func(line string)
	// OnProgress receives parsed progress updates.
	OnProgress func(Progress)
	Grace      time.Duration
}

// Result is the outcome of a call.
type Result struct {
	ExitCode    int
	Signal      string
	Stdout      []byte
	Errors      []string // "ERROR:" lines
	Warnings    []string // "WARNING:" lines
	Tail        []string // last lines of output
	Traceback   bool
	Interrupted bool
	StartErr    error
	Duration    time.Duration
}

// OK reports success.
func (r *Result) OK() bool {
	return r.StartErr == nil && !r.Interrupted && r.ExitCode == 0 && r.Signal == ""
}

const (
	maxStdout = 256 << 20
	tailLines = 40
)

// lineWriter splits a stream into lines on \n and \r (progress updates).
type lineWriter struct {
	mu   *sync.Mutex
	buf  []byte
	emit func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexAny(w.buf, "\r\n")
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		w.emit(line)
	}
	if len(w.buf) > 1<<20 { // pathological line without newline
		w.emit(string(w.buf))
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}

type limitedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxStdout {
		b.overflow = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// Run executes the call. It never returns before the process group is gone.
func Run(ctx context.Context, c Call) *Result {
	start := time.Now()
	res := &Result{}
	cmd := exec.Command(c.Bin, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "PYTHONIOENCODING=utf-8", "LC_ALL=C.UTF-8")
	cmd.Env = append(cmd.Env, c.Env...)

	var mu sync.Mutex
	handle := func(line string) {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			return
		}
		if p, ok := ParseProgress(line); ok {
			if c.OnProgress != nil {
				c.OnProgress(p)
			}
			return
		}
		switch {
		case strings.HasPrefix(line, "ERROR:"):
			res.Errors = append(res.Errors, line)
		case strings.HasPrefix(line, "WARNING:"):
			res.Warnings = append(res.Warnings, line)
		case strings.HasPrefix(line, "Traceback (most recent call last)"):
			res.Traceback = true
		}
		res.Tail = append(res.Tail, line)
		if len(res.Tail) > tailLines {
			res.Tail = res.Tail[len(res.Tail)-tailLines:]
		}
		if c.OnLine != nil {
			c.OnLine(line)
		}
	}
	stderr := &lineWriter{mu: &mu, emit: handle}
	cmd.Stderr = stderr
	var stdoutBuf *limitedBuffer
	var stdoutLines *lineWriter
	if c.CaptureStdout {
		stdoutBuf = &limitedBuffer{}
		cmd.Stdout = stdoutBuf
	} else {
		stdoutLines = &lineWriter{mu: &mu, emit: handle}
		cmd.Stdout = stdoutLines
	}

	err := procutil.Run(ctx, cmd, c.Grace)
	stderr.flush()
	if stdoutLines != nil {
		stdoutLines.flush()
	}
	res.Duration = time.Since(start)
	if stdoutBuf != nil {
		if stdoutBuf.overflow {
			res.Errors = append(res.Errors, "ERROR: yt-dlp output exceeded the size limit")
			res.ExitCode = 1
		}
		res.Stdout = stdoutBuf.Bytes()
	}
	switch {
	case err == nil:
	case errors.Is(err, procutil.ErrInterrupted):
		res.Interrupted = true
		res.ExitCode = -1
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if sig, ok := procutil.Signaled(err); ok {
				res.Signal = sig.String()
				res.ExitCode = -1
			} else {
				res.ExitCode = ee.ExitCode()
			}
		} else if errors.Is(err, exec.ErrWaitDelay) {
			// Exited fine, but a grandchild kept the pipes open.
		} else {
			res.StartErr = err
			res.ExitCode = -1
		}
	}
	return res
}

// Version runs "yt-dlp --version".
func Version(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	r := Run(ctx, Call{Bin: bin, Args: VersionArgs(), CaptureStdout: true, Grace: 2 * time.Second})
	if !r.OK() {
		return "", fmt.Errorf("%s --version failed: %s", bin, r.Describe())
	}
	return strings.TrimSpace(string(r.Stdout)), nil
}

// Describe summarizes a failed result for logs.
func (r *Result) Describe() string {
	switch {
	case r.StartErr != nil:
		if errors.Is(r.StartErr, exec.ErrNotFound) || errors.Is(r.StartErr, syscall.ENOENT) {
			return "executable not found: " + r.StartErr.Error()
		}
		return r.StartErr.Error()
	case r.Interrupted:
		return "interrupted"
	case r.Signal != "":
		return "killed by signal " + r.Signal
	case len(r.Errors) > 0:
		return r.Errors[len(r.Errors)-1]
	case r.ExitCode != 0:
		return fmt.Sprintf("exit status %d", r.ExitCode)
	}
	return "ok"
}
