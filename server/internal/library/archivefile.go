package library

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FileKey reports whether a key belongs in .ytdlp-archive.txt: tools that
// read it know yt-dlp's keys, including "reddit <id>" for Reddit videos,
// but not the server's own image, gallery and post keys (Reddit concept
// §7.2).
func FileKey(key string) bool {
	return !strings.HasPrefix(key, "redditmedia ") && !strings.HasPrefix(key, "redditgallery ") &&
		!strings.HasPrefix(key, "redditpost ")
}

// ArchiveFile maintains the yt-dlp compatible .ytdlp-archive.txt that the
// ytdlp-nfo CLI and other tools share (§4.6). The SQLite archive is the
// source of truth; this file is kept in sync for compatibility.
type ArchiveFile struct {
	Path string
	mu   sync.Mutex
}

// Append adds a key that the file doesn't hold yet (a forced re-download
// finalizes a key again) and fsyncs the file.
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
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == key {
			return nil
		}
	}
	prefix := ""
	if len(data) > 0 && data[len(data)-1] != '\n' {
		prefix = "\n" // another tool left no trailing newline
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
