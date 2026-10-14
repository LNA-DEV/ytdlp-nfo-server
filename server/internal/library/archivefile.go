package library

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ArchiveFile maintains the yt-dlp compatible .ytdlp-archive.txt that the
// ytdlp-nfo CLI and other tools share (§4.6). The SQLite archive is the
// source of truth; this file is kept in sync for compatibility.
type ArchiveFile struct {
	Path string
	mu   sync.Mutex
}

// Append adds a key and fsyncs the file.
func (a *ArchiveFile) Append(key string) error {
	if a == nil || a.Path == "" || key == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(a.Path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			prefix = "\n" // another tool left no trailing newline
		}
	}
	if _, err := f.WriteString(prefix + key + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

// Remove rewrites the file without key (atomic replace).
func (a *ArchiveFile) Remove(key string) error { return a.RemoveMany([]string{key}) }

// RemoveMany rewrites the file without the given keys in one atomic replace.
func (a *ArchiveFile) RemoveMany(keys []string) error {
	if a == nil || a.Path == "" || len(keys) == 0 {
		return nil
	}
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	data, err := os.ReadFile(a.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var out bytes.Buffer
	changed := false
	for _, line := range strings.Split(string(data), "\n") {
		if drop[strings.TrimSpace(line)] {
			changed = true
			continue
		}
		if line != "" {
			out.WriteString(line + "\n")
		}
	}
	if !changed {
		return nil
	}
	return WriteFileAtomic(a.Path, out.Bytes(), 0o644)
}
