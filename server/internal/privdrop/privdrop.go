// Package privdrop applies UMASK and drops root privileges to PUID/PGID so
// library files get the right owner for media servers (§12.1, F20).
package privdrop

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Config describes the target identity and the directories it must own.
type Config struct {
	UID, GID int
	Umask    *int
	// Recursive directories are chowned completely (data dir, work dirs).
	Recursive []string
	// Shallow directories only get their own owner changed (library roots;
	// existing library content is not touched).
	Shallow []string
}

// Apply sets the umask, chowns the directories and switches to UID/GID.
// With UID and GID 0 it only applies the umask.
func Apply(c Config, logf func(string, ...any)) error {
	if c.Umask != nil {
		syscall.Umask(*c.Umask)
	}
	if c.UID == 0 && c.GID == 0 {
		return nil
	}
	if os.Geteuid() != 0 {
		if os.Geteuid() == c.UID {
			return nil
		}
		return fmt.Errorf("PUID/PGID set to %d:%d but the server does not run as root", c.UID, c.GID)
	}
	for _, d := range c.Recursive {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		err := filepath.WalkDir(d, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			return os.Lchown(p, c.UID, c.GID)
		})
		if err != nil {
			return fmt.Errorf("chown %s: %w", d, err)
		}
	}
	for _, d := range c.Shallow {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		if err := os.Lchown(d, c.UID, c.GID); err != nil {
			return fmt.Errorf("chown %s: %w", d, err)
		}
	}
	if err := syscall.Setgroups([]int{c.GID}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(c.GID); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(c.UID); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	if logf != nil {
		logf("running as uid %d gid %d", c.UID, c.GID)
	}
	return nil
}
