// Package testutil builds the fake yt-dlp binary for tests.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	once     sync.Once
	fakeDir  string
	buildErr error
)

// FakeBin builds internal/testutil/fakeytdlp once per test process and
// returns a directory containing yt-dlp, ffprobe and ffmpeg (symlinks to
// the same binary). The caller should remove the directory in TestMain.
func FakeBin() (string, error) {
	once.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		src := filepath.Join(filepath.Dir(file), "fakeytdlp")
		fakeDir, buildErr = os.MkdirTemp("", "fakeytdlp-")
		if buildErr != nil {
			return
		}
		bin := filepath.Join(fakeDir, "yt-dlp")
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Dir = src
		cmd.Stderr = os.Stderr
		if buildErr = cmd.Run(); buildErr != nil {
			return
		}
		for _, name := range []string{"ffprobe", "ffmpeg"} {
			if buildErr = os.Symlink(bin, filepath.Join(fakeDir, name)); buildErr != nil {
				return
			}
		}
	})
	return fakeDir, buildErr
}

// NightlyCopy creates a second "installation" of the fake (a symlink in its
// own directory with a version file), used for nightly-retry tests.
func NightlyCopy(dir, version string) (string, error) {
	fake, err := FakeBin()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "yt-dlp")
	os.Remove(bin)
	if err := os.Symlink(filepath.Join(fake, "yt-dlp"), bin); err != nil {
		return "", err
	}
	return bin, os.WriteFile(filepath.Join(dir, "version"), []byte(version), 0o644)
}

// Cleanup removes the fake binary directory if it was built.
func Cleanup() {
	if fakeDir != "" {
		os.RemoveAll(fakeDir)
	}
}
