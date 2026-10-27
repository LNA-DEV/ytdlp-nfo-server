package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/buildinfo"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/pipeline"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// --- archive ---

// RemoveArchive deletes an archive entry (DB and legacy file) so the video
// can be downloaded again.
func (m *Manager) RemoveArchive(ctx context.Context, key string) error {
	if err := m.Store.RemoveArchive(ctx, key); err != nil {
		return err
	}
	if err := m.Archive.Remove(key); err != nil {
		m.logf("archive: could not update %s: %v", m.Archive.Path, err)
	}
	m.markCounts(0)
	return nil
}

// ImportArchive adds keys from a yt-dlp archive file.
func (m *Manager) ImportArchive(ctx context.Context, data []byte) (int, error) {
	keys, err := store.ParseArchive(bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	n, err := m.Store.ImportArchiveKeys(ctx, keys, "import")
	if err != nil {
		return 0, err
	}
	if n > 0 { // keep the shared legacy file in sync
		m.markCounts(0)

		all, err := m.Store.AllArchiveKeys(ctx)
		if err == nil {
			var b strings.Builder
			for _, k := range all {
				b.WriteString(k + "\n")
			}
			_ = library.WriteFileAtomic(m.Archive.Path, []byte(b.String()), 0o644)
		}
	}
	return n, nil
}

// ExportArchive returns the archive in yt-dlp format.
func (m *Manager) ExportArchive(ctx context.Context) ([]byte, error) {
	keys, err := m.Store.AllArchiveKeys(ctx)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for _, k := range keys {
		b.WriteString(k + "\n")
	}
	return b.Bytes(), nil
}

// --- site rules ---

// SiteRules lists rules.
func (m *Manager) SiteRules() []config.SiteRule { return m.siteRules() }

// SaveSiteRule creates or updates a rule.
func (m *Manager) SaveSiteRule(ctx context.Context, r config.SiteRule) (config.SiteRule, error) {
	if err := r.Validate(); err != nil {
		return r, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	s := m.Settings.Settings()
	if r.Preset != "" {
		if _, ok := s.Presets[r.Preset]; !ok {
			return r, fmt.Errorf("%w: unknown preset %q", ErrInvalid, r.Preset)
		}
	}
	if r.Library != "" && !s.HasLibrary(r.Library) {
		return r, fmt.Errorf("%w: unknown library %q", ErrInvalid, r.Library)
	}
	if r.CookieProfile != "" {
		if _, err := m.Store.GetCookieProfile(ctx, r.CookieProfile); err != nil {
			return r, fmt.Errorf("%w: unknown cookie profile %q", ErrInvalid, r.CookieProfile)
		}
	}
	if err := m.Store.SaveSiteRule(ctx, &r); err != nil {
		return r, err
	}
	return r, m.reloadRules(ctx)
}

// DeleteSiteRule removes a rule.
func (m *Manager) DeleteSiteRule(ctx context.Context, id int64) error {
	if err := m.Store.DeleteSiteRule(ctx, id); err != nil {
		return err
	}
	return m.reloadRules(ctx)
}

// --- cookies ---

// SaveCookieProfile stores a Netscape cookies.txt with 0600 permissions.
// The content is never returned by the API.
func (m *Manager) SaveCookieProfile(ctx context.Context, name string, data []byte) (*store.CookieProfile, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 || strings.ContainsAny(name, "/\\\x00") {
		return nil, fmt.Errorf("%w: invalid profile name", ErrInvalid)
	}
	if len(data) == 0 || len(data) > 4<<20 {
		return nil, fmt.Errorf("%w: cookie file must be between 1 byte and 4 MB", ErrInvalid)
	}
	first := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if !strings.HasPrefix(first, "# Netscape HTTP Cookie File") && !strings.HasPrefix(first, "# HTTP Cookie File") && strings.Count(string(data), "\t") < 6 {
		return nil, fmt.Errorf("%w: not a Netscape cookies.txt file", ErrInvalid)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	file := "profile-" + hex.EncodeToString(b) + ".txt"
	path := filepath.Join(m.cookieDir, file)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	old, _ := m.Store.GetCookieProfile(ctx, name)
	p := &store.CookieProfile{Name: name, Filename: file, Size: int64(len(data))}
	if err := m.Store.SaveCookieProfile(ctx, p); err != nil {
		os.Remove(path)
		return nil, err
	}
	if old != nil && old.Filename != file {
		os.Remove(filepath.Join(m.cookieDir, filepath.Base(old.Filename)))
	}
	return m.Store.GetCookieProfile(ctx, name)
}

// DeleteCookieProfile removes a profile and its file.
func (m *Manager) DeleteCookieProfile(ctx context.Context, name string) error {
	p, err := m.Store.GetCookieProfile(ctx, name)
	if err != nil {
		return err
	}
	if err := m.Store.DeleteCookieProfile(ctx, name); err != nil {
		return err
	}
	os.Remove(filepath.Join(m.cookieDir, filepath.Base(p.Filename)))
	return nil
}

// --- tokens ---

// HashToken returns the stored hash of an API token or session id.
func HashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// CreateToken creates an API token; the plain value is returned once.
func (m *Manager) CreateToken(ctx context.Context, name string) (string, *store.APIToken, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return "", nil, fmt.Errorf("%w: token name is required", ErrInvalid)
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	plain := "ytn_" + hex.EncodeToString(b)
	t := &store.APIToken{Name: name, Hash: HashToken(plain), Prefix: plain[:10]}
	if err := m.Store.CreateToken(ctx, t); err != nil {
		return "", nil, err
	}
	return plain, t, nil
}

// --- system ---

// QueueStatus is the queue part of the system status.
type QueueStatus struct {
	scheduler.Status
	Counts map[string]int `json:"counts"`
}

// YtdlpStatus describes the active yt-dlp.
type YtdlpStatus struct {
	Active                 *store.YtdlpVersion `json:"active,omitempty"`
	Channel                string              `json:"channel"`
	Updates                map[string]string   `json:"updates,omitempty"`
	ImpersonationAvailable bool                `json:"impersonationAvailable"`
	Targets                []string            `json:"targets,omitempty"`
}

// QuickStatusView is the payload of "system" events (cheap to compute).
type QuickStatusView struct {
	Server   map[string]string `json:"server"`
	Ytdlp    YtdlpStatus       `json:"ytdlp"`
	Queue    QueueStatus       `json:"queue"`
	Warnings []Warning         `json:"warnings"`
}

// QuickStatus returns the header/banner state.
func (m *Manager) QuickStatus(ctx context.Context) QuickStatusView {
	v, c, d := buildinfo.Info()
	out := QuickStatusView{Server: map[string]string{"version": v, "commit": c, "date": d}}
	s := m.Settings.Settings()
	out.Ytdlp.Channel = s.Ytdlp.Channel
	if act, err := m.Versions.Active(ctx); err == nil {
		out.Ytdlp.Active = act
		out.Ytdlp.Targets = act.ImpersonateTargets
		out.Ytdlp.ImpersonationAvailable = len(act.ImpersonateTargets) > 0
		if u := m.Versions.Updates(); u[act.Channel] != "" {
			out.Ytdlp.Updates = u
		}
	}
	out.Queue.Status = m.Sched.Status()
	out.Queue.Counts, _ = m.Store.CountJobsByTab(ctx)
	m.mu.Lock()
	for _, w := range m.warnings {
		out.Warnings = append(out.Warnings, w)
	}
	m.mu.Unlock()
	if out.Warnings == nil {
		out.Warnings = []Warning{}
	}
	return out
}

// DiskInfo is free space per directory.
type DiskInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Free uint64 `json:"free"`
}

// SystemView is the diagnostics payload (GET /api/v1/system).
type SystemView struct {
	QuickStatusView
	Tools     map[string]string `json:"tools"`
	Disk      []DiskInfo        `json:"disk"`
	DataDir   string            `json:"dataDir"`
	Persisted bool              `json:"persisted"`
	Libraries []config.Library  `json:"libraries"`
}

// System returns diagnostics.
func (m *Manager) System(ctx context.Context) SystemView {
	sv := SystemView{QuickStatusView: m.QuickStatus(ctx), Tools: pipeline.ToolVersions(m.Env), DataDir: m.dataDir,
		Persisted: !m.Store.InMemory()}
	dirs := []DiskInfo{{Name: "data", Path: m.dataDir}, {Name: "downloads", Path: m.Env.DownloadDir}}
	sv.Libraries = append(sv.Libraries, config.Library{Name: config.DefaultLibrary, Path: m.Env.LibraryRoot()})
	for _, l := range m.Settings.Settings().Libraries {
		sv.Libraries = append(sv.Libraries, l)
	}
	for _, l := range sv.Libraries {
		dirs = append(dirs, DiskInfo{Name: "library " + l.Name, Path: l.Path})
	}
	for i := range dirs {
		dirs[i].Free, _ = library.FreeBytes(dirs[i].Path)
	}
	sv.Disk = dirs
	return sv
}

// TestURL runs a probe with the active yt-dlp and returns the raw output
// (diagnostics "Test URL").
func (m *Manager) TestURL(ctx context.Context, url string, impersonate string) (map[string]any, error) {
	if err := config.ValidateURL(url); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := config.ValidateImpersonate(impersonate); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	bin, release, err := m.resolver.Acquire(ctx, "")
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var lines []string
	args := ytdlp.ProbeArgs(url, ytdlp.CallOptions{Impersonate: impersonate, CacheDir: filepath.Join(m.dataDir, "ytdlp", "cache")})
	res := ytdlp.Run(ctx, ytdlp.Call{Bin: bin.Path, Args: args, CaptureStdout: true, OnLine: func(l string) { lines = append(lines, l) }})
	out := map[string]any{
		"ytdlp":    bin.ID,
		"command":  append([]string{filepath.Base(bin.Path)}, args...),
		"exitCode": res.ExitCode,
		"output":   lines,
		"duration": res.Duration.Seconds(),
	}
	if !res.OK() {
		c := ytdlp.Classify(res)
		out["class"], out["reason"], out["hint"] = c.Class, c.Reason, c.Hint
	}
	if pr, err := ytdlp.ParseProbe(res.Stdout); err == nil {
		entries := make([]map[string]string, 0, min(len(pr.Entries), 50))
		for i, e := range pr.Entries {
			if i >= 50 {
				break
			}
			entries = append(entries, map[string]string{"title": e.Title, "url": e.URL, "key": e.Key()})
		}
		out["result"] = map[string]any{"playlist": pr.IsPlaylist, "title": pr.Title, "uploader": pr.Uploader,
			"entries": entries, "count": len(pr.Entries)}
	}
	return out, nil
}

// IsNotFound reports whether err means a missing record.
func IsNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
