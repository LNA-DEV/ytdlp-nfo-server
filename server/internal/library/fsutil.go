package library

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ErrExchangeUnsupported is returned by FS.Exchange when the filesystem
// cannot swap two paths atomically.
var ErrExchangeUnsupported = errors.New("atomic exchange not supported")

// FS is the set of filesystem operations finalize uses. Tests wrap it to
// inject faults (disk full, EXDEV, permission denied).
type FS interface {
	Rename(oldpath, newpath string) error
	// Exchange atomically swaps two existing paths (renameat2 RENAME_EXCHANGE).
	Exchange(a, b string) error
	Mkdir(path string, perm os.FileMode) error
	// CopyFile copies src to a new file dst and fsyncs it.
	CopyFile(src, dst string) error
	// Sync fsyncs a file or directory.
	Sync(path string) error
	WriteFileSync(path string, data []byte, perm os.FileMode) error
	RemoveAll(path string) error
	Remove(path string) error
}

// OSFS is the real filesystem.
type OSFS struct{}

func (OSFS) Rename(o, n string) error { return os.Rename(o, n) }

func (OSFS) Exchange(a, b string) error {
	err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) {
		return ErrExchangeUnsupported
	}
	return err
}

func (OSFS) Mkdir(p string, perm os.FileMode) error { return os.Mkdir(p, perm) }
func (OSFS) RemoveAll(p string) error               { return os.RemoveAll(p) }
func (OSFS) Remove(p string) error                  { return os.Remove(p) }
func (OSFS) Sync(p string) error                    { return SyncPath(p) }

func (OSFS) CopyFile(src, dst string) error { return CopyFileSync(src, dst) }

func (OSFS) WriteFileSync(p string, data []byte, perm os.FileMode) error {
	return WriteFileSync(p, data, perm)
}

// SyncPath fsyncs a file or directory.
func SyncPath(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

// WriteFileSync writes data to p (truncating) and fsyncs it.
func WriteFileSync(p string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// WriteFileAtomic writes p via a temp file, fsync and rename, then fsyncs the
// parent directory, so readers see either the old or the new content.
func WriteFileAtomic(p string, data []byte, perm os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(p), "."+filepath.Base(p)+".tmp")
	if err := WriteFileSync(tmp, data, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return SyncPath(filepath.Dir(p))
}

// CopyFileSync copies src to dst (replacing a partial dst), keeps the
// modification time of src and fsyncs dst.
func CopyFileSync(src, dst string) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	info, err := sf.Stat()
	if err != nil {
		return err
	}
	df, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(df, sf); err != nil {
		df.Close()
		return err
	}
	// Keep the modification time, as a rename does: Reddit media carry
	// the post time (Reddit concept §8). The fsync below persists it.
	if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		df.Close()
		return err
	}
	if err := df.Sync(); err != nil {
		df.Close()
		return err
	}
	if err := df.Close(); err != nil {
		return err
	}
	di, err := os.Stat(dst)
	if err != nil {
		return err
	}
	if di.Size() != info.Size() {
		return fmt.Errorf("copy %s: size mismatch (%d != %d)", src, di.Size(), info.Size())
	}
	return nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// MkdirAllSync creates rel below root, fsyncing the parent of every
// directory it creates so the new entries survive a crash.
func MkdirAllSync(fs FS, root, rel string) error {
	cur := root
	for _, c := range strings.Split(filepath.ToSlash(rel), "/") {
		if c == "" || c == "." {
			continue
		}
		if c == ".." {
			return fmt.Errorf("invalid path %q", rel)
		}
		next := filepath.Join(cur, c)
		if fi, err := os.Stat(next); err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", next)
			}
		} else {
			if err := fs.Mkdir(next, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			if err := fs.Sync(cur); err != nil {
				return err
			}
		}
		cur = next
	}
	return nil
}

// FreeBytes returns the bytes available to unprivileged users at p.
func FreeBytes(p string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// SameFilesystem reports whether a and b are on the same device.
func SameFilesystem(a, b string) bool {
	var sa, sb unix.Stat_t
	if unix.Stat(a, &sa) != nil || unix.Stat(b, &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev
}
