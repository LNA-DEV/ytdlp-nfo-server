// Package config holds the process environment configuration and the
// runtime settings that are editable in the UI and persisted in the store.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Env is the configuration read from environment variables at start.
type Env struct {
	Port        string
	DataDir     string // empty: in-memory DB, temp dir for logs etc.
	DownloadDir string
	OutputDir   string // empty: finalize into DownloadDir
	Password    string

	// Explicitly set values that override (and lock) runtime settings.
	MaxConcurrent *int
	MaxRetries    *int
	YtdlpChannel  string

	PUID  int
	PGID  int
	Umask *int

	GitHubToken            string
	JellyfinURL            string
	JellyfinAPIKey         string
	TrustedProxyAuthHeader string
	Libraries              []Library // from LIBRARIES="Name=/path;Other=/path2"

	// Paths to external tools. YtdlpPath overrides the version manager.
	YtdlpPath      string
	FFmpegPath     string
	FFprobePath    string
	DenoPath       string
	BundledYtdlDir string
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string) (*int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return &n, nil
}

// LoadEnv reads the environment.
func LoadEnv() (*Env, error) {
	e := &Env{
		Port:                   getenv("PORT", "8080"),
		DataDir:                os.Getenv("DATA_DIR"),
		DownloadDir:            getenv("DOWNLOAD_DIR", "./downloads"),
		OutputDir:              os.Getenv("OUTPUT_DIR"),
		Password:               os.Getenv("PASSWORD"),
		YtdlpChannel:           strings.ToLower(strings.TrimSpace(os.Getenv("YTDLP_CHANNEL"))),
		GitHubToken:            os.Getenv("GITHUB_TOKEN"),
		JellyfinURL:            strings.TrimRight(os.Getenv("JELLYFIN_URL"), "/"),
		JellyfinAPIKey:         os.Getenv("JELLYFIN_API_KEY"),
		TrustedProxyAuthHeader: os.Getenv("TRUSTED_PROXY_AUTH_HEADER"),
		YtdlpPath:              os.Getenv("YTDLP_PATH"),
		FFmpegPath:             getenv("FFMPEG_PATH", "ffmpeg"),
		FFprobePath:            getenv("FFPROBE_PATH", "ffprobe"),
		DenoPath:               getenv("DENO_PATH", "deno"),
		BundledYtdlDir:         getenv("YTDLP_BUNDLED_DIR", "/opt/ytdlp/bundled"),
	}
	var err error
	if e.MaxConcurrent, err = envInt("MAX_CONCURRENT"); err != nil {
		return nil, err
	}
	if e.MaxConcurrent != nil && *e.MaxConcurrent < 1 {
		e.MaxConcurrent = nil
	}
	if e.MaxRetries, err = envInt("MAX_RETRIES"); err != nil {
		return nil, err
	}
	if e.MaxRetries != nil && *e.MaxRetries < 0 {
		e.MaxRetries = nil
	}
	if e.YtdlpChannel != "" && !ValidChannel(e.YtdlpChannel) {
		return nil, fmt.Errorf("YTDLP_CHANNEL: unknown channel %q (stable, nightly, master)", e.YtdlpChannel)
	}
	if p, err := envInt("PUID"); err != nil {
		return nil, err
	} else if p != nil {
		e.PUID = *p
	}
	if p, err := envInt("PGID"); err != nil {
		return nil, err
	} else if p != nil {
		e.PGID = *p
	}
	if v := strings.TrimSpace(os.Getenv("UMASK")); v != "" {
		n, err := strconv.ParseInt(v, 8, 32)
		if err != nil {
			return nil, fmt.Errorf("UMASK: %w", err)
		}
		m := int(n)
		e.Umask = &m
	}
	libs, err := ParseLibraries(os.Getenv("LIBRARIES"))
	if err != nil {
		return nil, err
	}
	e.Libraries = libs
	return e, nil
}

// ParseLibraries parses "Name=/path;Other=/path2".
func ParseLibraries(s string) ([]Library, error) {
	var out []Library
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, path, ok := strings.Cut(part, "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !ok || name == "" || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("LIBRARIES: invalid entry %q (want Name=/absolute/path)", part)
		}
		out = append(out, Library{Name: name, Path: filepath.Clean(path)})
	}
	return out, nil
}

// LibraryRoot is where finished items of the default library go.
func (e *Env) LibraryRoot() string {
	if e.OutputDir != "" {
		return e.OutputDir
	}
	return e.DownloadDir
}

// LegacyArchivePath is the yt-dlp style archive shared with other tools.
func (e *Env) LegacyArchivePath() string {
	return filepath.Join(e.DownloadDir, ".ytdlp-archive.txt")
}

// WorkRoot is the parent of all per-item work directories.
func (e *Env) WorkRoot() string {
	return filepath.Join(e.DownloadDir, ".work")
}
