// Package joblog writes per-job log files (DATA_DIR/logs/<job>.log), capped
// at a maximum size with head and tail kept, and streams new lines to
// subscribers (§8.2, §10, F12).
package joblog

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMax = 2 << 20
	headKeep   = 512 << 10
)

// Manager owns all job logs.
type Manager struct {
	Dir     string
	MaxSize int64
	// TailOnly drops the head when a log is capped (subscription check
	// logs: only the latest checks matter).
	TailOnly bool

	mu   sync.Mutex
	logs map[int64]*Log
}

// New creates a manager writing to dir.
func New(dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Manager{Dir: dir, MaxSize: defaultMax, logs: map[int64]*Log{}}, nil
}

// Path returns the log file of a job.
func (m *Manager) Path(jobID int64) string {
	return filepath.Join(m.Dir, strconv.FormatInt(jobID, 10)+".log")
}

// Get returns the log of a job, creating its state if needed.
func (m *Manager) Get(jobID int64) *Log {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.logs[jobID]
	if !ok {
		l = &Log{path: m.Path(jobID), max: m.MaxSize, tailOnly: m.TailOnly, subs: map[chan string]struct{}{}}
		m.logs[jobID] = l
	}
	return l
}

// Note appends a line for a job that is not necessarily running and closes
// the file again, so queued jobs don't hold file descriptors.
func (m *Manager) Note(jobID int64, format string, args ...any) {
	l := m.Get(jobID)
	l.Printf(format, args...)
	l.closeFile()
}

// Release closes the file handle of a finished job (subscribers stay).
func (m *Manager) Release(jobID int64) {
	m.mu.Lock()
	l, ok := m.logs[jobID]
	m.mu.Unlock()
	if ok {
		l.closeFile()
	}
}

// Delete removes a job's log and disconnects its subscribers.
func (m *Manager) Delete(jobID int64) {
	m.mu.Lock()
	l, ok := m.logs[jobID]
	delete(m.logs, jobID)
	m.mu.Unlock()
	if ok {
		l.closeAll()
	}
	os.Remove(m.Path(jobID))
}

// Read returns the full persisted log.
func (m *Manager) Read(jobID int64) ([]byte, error) {
	l := m.Get(jobID)
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := os.ReadFile(l.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}

// Tail returns the last n lines.
func (m *Manager) Tail(jobID int64, n int) []string {
	b, _ := m.Read(jobID)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// Subscribe returns the current content and a channel of new lines.
func (m *Manager) Subscribe(jobID int64) ([]byte, chan string, func()) {
	l := m.Get(jobID)
	l.mu.Lock()
	snapshot, _ := os.ReadFile(l.path)
	ch := make(chan string, 512)
	l.subs[ch] = struct{}{}
	l.mu.Unlock()
	var once sync.Once
	return snapshot, ch, func() {
		once.Do(func() {
			l.mu.Lock()
			if _, ok := l.subs[ch]; ok {
				delete(l.subs, ch)
				close(ch)
			}
			l.mu.Unlock()
		})
	}
}

// Log is one job's log.
type Log struct {
	mu       sync.Mutex
	path     string
	max      int64
	tailOnly bool
	f        *os.File
	size     int64
	subs     map[chan string]struct{}
}

// Line appends one line (timestamped).
func (l *Log) Line(s string) {
	s = strings.ReplaceAll(strings.TrimRight(s, "\r\n"), "\n", " ")
	line := time.Now().Format("2006-01-02 15:04:05") + " " + s
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.open(); err == nil {
		n, _ := l.f.WriteString(line + "\n")
		l.size += int64(n)
		if l.size > l.max {
			l.compact()
		}
	}
	for ch := range l.subs {
		select {
		case ch <- line:
		default: // slow consumer: drop
		}
	}
}

// Printf appends a formatted line.
func (l *Log) Printf(format string, args ...any) { l.Line(fmt.Sprintf(format, args...)) }

func (l *Log) open() error {
	if l.f != nil {
		return nil
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *Log) closeFile() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

func (l *Log) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	for ch := range l.subs {
		close(ch)
		delete(l.subs, ch)
	}
}

// compact keeps the head and the tail of the log (caller holds l.mu).
func (l *Log) compact() {
	l.f.Close()
	l.f = nil
	data, err := os.ReadFile(l.path)
	if err != nil {
		_ = l.open()
		return
	}
	headSize := min(headKeep, int(l.max)/4)
	if l.tailOnly {
		headSize = 0
	}
	tailKeep := int(l.max) - headSize - 1024
	if tailKeep <= 0 || len(data) <= headSize+tailKeep {
		_ = l.open()
		return
	}
	head := data[:headSize]
	if i := bytes.LastIndexByte(head, '\n'); i > 0 && headSize > 0 {
		head = head[:i+1]
	}
	tail := data[len(data)-tailKeep:]
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	var buf bytes.Buffer
	buf.Write(head)
	fmt.Fprintf(&buf, "%s ... [%d bytes of log truncated] ...\n", time.Now().Format("2006-01-02 15:04:05"), len(data)-len(head)-len(tail))
	buf.Write(tail)
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o640); err != nil {
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return
	}
	_ = l.open()
}

// Lines splits log content into lines.
func Lines(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// Copy streams a log file to w.
func (m *Manager) Copy(jobID int64, w io.Writer) error {
	b, err := m.Read(jobID)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
