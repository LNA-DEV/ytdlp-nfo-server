package versions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// Binary is a resolved yt-dlp executable for one job.
type Binary struct {
	ID                   string
	Channel              string
	Tag                  string
	Version              string
	Path                 string
	Targets              []string // available impersonation targets
	ImpersonateAvailable bool
}

// Release is a published build.
type Release struct {
	Channel     string    `json:"channel"`
	Tag         string    `json:"tag"`
	PublishedAt time.Time `json:"publishedAt,omitzero"`
	Installed   bool      `json:"installed"`
	Active      bool      `json:"active"`
}

// SettingsAccess lets the manager read and update runtime settings.
type SettingsAccess interface {
	Settings() config.Settings
	UpdateYtdlp(ctx context.Context, fn func(*config.YtdlpSettings)) error
}

// Manager manages installed yt-dlp versions.
type Manager struct {
	Store      *store.Store
	Settings   SettingsAccess
	Fetcher    *Fetcher
	APIURL     string // default https://api.github.com
	Dir        string // DATA_DIR/ytdlp
	BundledDir string
	SystemPath string // YTDLP_PATH: use this binary, skip management
	Arch       string
	Logf       func(format string, args ...any)
	OnChange   func()

	mu        sync.Mutex
	cache     map[string]cachedReleases
	inUse     map[string]int
	installMu sync.Map          // id -> *sync.Mutex
	updates   map[string]string // channel -> newer tag available
	lastCheck time.Time
}

type cachedReleases struct {
	at       time.Time
	releases []Release
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

func (m *Manager) changed() {
	if m.OnChange != nil {
		m.OnChange()
	}
}

func (m *Manager) arch() string {
	if m.Arch != "" {
		return m.Arch
	}
	return runtime.GOARCH
}

func (m *Manager) init() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cache == nil {
		m.cache = map[string]cachedReleases{}
		m.inUse = map[string]int{}
		m.updates = map[string]string{}
	}
}

func versionID(channel, tag string) string { return channel + "/" + tag }

// --- listing ---

// Releases lists available versions of a channel (cached for 1 h). If the
// GitHub API is unavailable (e.g. rate limit) only the latest tag is
// returned, found via the releases/latest redirect.
func (m *Manager) Releases(ctx context.Context, channel string) ([]Release, error) {
	m.init()
	if _, ok := Repos[channel]; !ok {
		return nil, fmt.Errorf("unknown channel %q", channel)
	}
	m.mu.Lock()
	c, ok := m.cache[channel]
	m.mu.Unlock()
	var rels []Release
	if ok && time.Since(c.at) < time.Hour {
		rels = c.releases
	} else {
		var err error
		rels, err = m.fetchReleases(ctx, channel)
		if err != nil {
			m.logf("yt-dlp: listing %s releases via API failed (%v), falling back to latest", channel, err)
			tag, lerr := m.latestViaRedirect(ctx, channel)
			if lerr != nil {
				if ok {
					rels = c.releases // stale cache is better than nothing
				} else {
					return nil, fmt.Errorf("list releases: %w; latest: %v", err, lerr)
				}
			} else {
				rels = []Release{{Channel: channel, Tag: tag}}
			}
		}
		m.mu.Lock()
		m.cache[channel] = cachedReleases{at: time.Now(), releases: rels}
		m.mu.Unlock()
	}
	installed, _ := m.Store.ListVersions(ctx)
	active := m.Settings.Settings().Ytdlp.Active
	out := make([]Release, len(rels))
	for i, r := range rels {
		r.Installed = false
		for _, v := range installed {
			if v.Channel == channel && v.Tag == r.Tag {
				r.Installed = true
				r.Active = v.ID == active
			}
		}
		out[i] = r
	}
	return out, nil
}

func (m *Manager) apiBase() string {
	if m.APIURL != "" {
		return strings.TrimRight(m.APIURL, "/")
	}
	return "https://api.github.com"
}

func (m *Manager) fetchReleases(ctx context.Context, channel string) ([]Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=30", m.apiBase(), Repos[channel])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ytdlp-nfo-server")
	if m.Fetcher.Token != "" {
		req.Header.Set("Authorization", "Bearer "+m.Fetcher.Token)
	}
	resp, err := m.Fetcher.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API: %s", resp.Status)
	}
	var raw []struct {
		TagName     string    `json:"tag_name"`
		PublishedAt time.Time `json:"published_at"`
		Draft       bool      `json:"draft"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	var rels []Release
	for _, r := range raw {
		if r.Draft || !validTag(r.TagName) {
			continue
		}
		rels = append(rels, Release{Channel: channel, Tag: r.TagName, PublishedAt: r.PublishedAt})
	}
	sort.Slice(rels, func(i, j int) bool { return CompareTags(rels[i].Tag, rels[j].Tag) > 0 })
	return rels, nil
}

func (m *Manager) latestViaRedirect(ctx context.Context, channel string) (string, error) {
	return m.Fetcher.LatestTag(ctx, channel)
}

// LatestTag returns the newest tag of a channel.
func (m *Manager) LatestTag(ctx context.Context, channel string) (string, error) {
	rels, err := m.Releases(ctx, channel)
	if err != nil {
		return "", err
	}
	if len(rels) == 0 {
		return "", fmt.Errorf("no releases for %s", channel)
	}
	return rels[0].Tag, nil
}

// --- install ---

func (m *Manager) lockID(id string) func() {
	v, _ := m.installMu.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Install downloads, verifies and smoke-tests a version. An installed
// version whose smoke test failed is recorded but cannot be activated.
func (m *Manager) Install(ctx context.Context, channel, tag string) (*store.YtdlpVersion, error) {
	m.init()
	if !config.ValidChannel(channel) || !validTag(tag) {
		return nil, fmt.Errorf("invalid version %s/%s", channel, tag)
	}
	id := versionID(channel, tag)
	defer m.lockID(id)()
	if v, err := m.Store.GetVersion(ctx, id); err == nil {
		if _, err := os.Stat(v.Path); err == nil {
			return v, nil
		}
	}
	final := filepath.Join(m.Dir, channel, tag)
	tmp := filepath.Join(m.Dir, channel, ".tmp-"+tag)
	os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Dir(tmp), 0o755); err != nil {
		return nil, err
	}
	m.logf("yt-dlp: installing %s", id)
	man, err := m.Fetcher.Fetch(ctx, channel, tag, m.arch(), tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	v := &store.YtdlpVersion{
		ID: id, Channel: channel, Tag: tag, Dir: final,
		Path: filepath.Join(final, man.Exe), Asset: man.Asset, SHA256: man.SHA256,
	}
	m.smoke(ctx, v)
	if err := m.Store.UpsertVersion(ctx, v); err != nil {
		return nil, err
	}
	m.logf("yt-dlp: installed %s (version %s, smoke test ok=%v)", id, v.Version, v.SmokeOK)
	m.changed()
	m.GC(context.WithoutCancel(ctx))
	return v, nil
}

// smoke runs the smoke test and records the result on v (§6.2).
func (m *Manager) smoke(ctx context.Context, v *store.YtdlpVersion) {
	var out strings.Builder
	v.SmokeOK = false
	ver, err := ytdlp.Version(ctx, v.Path)
	if err != nil {
		fmt.Fprintf(&out, "--version failed: %v\n", err)
		v.SmokeOutput = out.String()
		return
	}
	v.Version = ver
	fmt.Fprintf(&out, "version: %s\n", ver)
	targets, raw, err := ytdlp.ListTargets(ctx, v.Path)
	if err != nil {
		fmt.Fprintf(&out, "--list-impersonate-targets failed: %v\n", err)
		v.SmokeOutput = out.String()
		return
	}
	v.ImpersonateTargets = nil
	for _, t := range targets {
		if t.Available {
			v.ImpersonateTargets = append(v.ImpersonateTargets, t.Name())
		}
	}
	fmt.Fprintf(&out, "impersonate targets available: %d of %d\n", len(v.ImpersonateTargets), len(targets))
	_ = raw
	if u := m.Settings.Settings().Ytdlp.SmokeTestURL; u != "" {
		sctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		res := ytdlp.Run(sctx, ytdlp.Call{Bin: v.Path, Args: ytdlp.SimulateArgs(u), Grace: 5 * time.Second})
		cancel()
		if !res.OK() {
			fmt.Fprintf(&out, "--simulate %s failed: %s\n", u, res.Describe())
			v.SmokeOutput = out.String()
			return
		}
		fmt.Fprintf(&out, "--simulate ok\n")
	}
	v.SmokeOK = true
	v.SmokeOutput = out.String()
}

// Retest re-runs the smoke test of an installed version.
func (m *Manager) Retest(ctx context.Context, id string) (*store.YtdlpVersion, error) {
	v, err := m.Store.GetVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	m.smoke(ctx, v)
	if err := m.Store.UpsertVersion(ctx, v); err != nil {
		return nil, err
	}
	m.changed()
	return v, nil
}

// --- activation ---

// Activate makes a version the default for new jobs. Running jobs keep
// the binary they resolved when they started.
func (m *Manager) Activate(ctx context.Context, id string) error {
	v, err := m.Store.GetVersion(ctx, id)
	if err != nil {
		return err
	}
	if !v.SmokeOK {
		return fmt.Errorf("version %s failed its smoke test and cannot be activated", id)
	}
	err = m.Settings.UpdateYtdlp(ctx, func(y *config.YtdlpSettings) {
		y.Active = id
		if config.ValidChannel(v.Channel) {
			y.Channel = v.Channel
		}
	})
	if err != nil {
		return err
	}
	m.logf("yt-dlp: activated %s", id)
	m.mu.Lock()
	if m.updates != nil && CompareTags(v.Tag, m.updates[v.Channel]) >= 0 {
		delete(m.updates, v.Channel)
	}
	m.mu.Unlock()
	m.changed()
	return nil
}

// SetPinned pins a version so retention never removes it.
func (m *Manager) SetPinned(ctx context.Context, id string, pinned bool) error {
	v, err := m.Store.GetVersion(ctx, id)
	if err != nil {
		return err
	}
	v.Pinned = pinned
	return m.Store.UpsertVersion(ctx, v)
}

// Delete removes an installed version unless it is active, bundled,
// system-provided or in use.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.init()
	v, err := m.Store.GetVersion(ctx, id)
	if err != nil {
		return err
	}
	if v.ID == m.Settings.Settings().Ytdlp.Active {
		return fmt.Errorf("cannot delete the active version")
	}
	if v.Bundled || v.Channel == "system" {
		return fmt.Errorf("bundled and system versions cannot be deleted")
	}
	m.mu.Lock()
	busy := m.inUse[id] > 0
	m.mu.Unlock()
	if busy {
		return fmt.Errorf("version %s is used by a running job", id)
	}
	if err := m.Store.DeleteVersion(ctx, id); err != nil {
		return err
	}
	if v.Dir != "" && strings.HasPrefix(filepath.Clean(v.Dir), filepath.Clean(m.Dir)+string(os.PathSeparator)) {
		os.RemoveAll(v.Dir)
	}
	m.changed()
	return nil
}

// GC applies retention: keep the active, pinned, bundled and in-use
// versions and the newest 3 per channel.
func (m *Manager) GC(ctx context.Context) {
	m.init()
	all, err := m.Store.ListVersions(ctx)
	if err != nil {
		return
	}
	active := m.Settings.Settings().Ytdlp.Active
	byChannel := map[string][]*store.YtdlpVersion{}
	for _, v := range all {
		if v.Bundled || v.Channel == "system" {
			continue
		}
		byChannel[v.Channel] = append(byChannel[v.Channel], v)
	}
	for _, vs := range byChannel {
		sort.Slice(vs, func(i, j int) bool { return CompareTags(vs[i].Tag, vs[j].Tag) > 0 })
		for i, v := range vs {
			if i < 3 || v.Pinned || v.ID == active {
				continue
			}
			m.mu.Lock()
			busy := m.inUse[v.ID] > 0
			m.mu.Unlock()
			if busy {
				continue
			}
			m.logf("yt-dlp: retention removes %s", v.ID)
			_ = m.Delete(ctx, v.ID)
		}
	}
}

// --- resolution ---

// Acquire resolves a version spec for a job ("" = active, "<channel>:latest",
// "<channel>/<tag>") and marks it in use until release is called.
func (m *Manager) Acquire(ctx context.Context, spec string) (Binary, func(), error) {
	m.init()
	v, err := m.resolve(ctx, spec)
	if err != nil {
		return Binary{}, func() {}, err
	}
	m.mu.Lock()
	m.inUse[v.ID]++
	m.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			m.mu.Lock()
			m.inUse[v.ID]--
			m.mu.Unlock()
		})
	}
	return Binary{
		ID: v.ID, Channel: v.Channel, Tag: v.Tag, Version: v.Version, Path: v.Path,
		Targets: v.ImpersonateTargets, ImpersonateAvailable: len(v.ImpersonateTargets) > 0,
	}, release, nil
}

func (m *Manager) resolve(ctx context.Context, spec string) (*store.YtdlpVersion, error) {
	if spec == "" {
		id := m.Settings.Settings().Ytdlp.Active
		if id == "" {
			return nil, fmt.Errorf("no yt-dlp version is active")
		}
		return m.Store.GetVersion(ctx, id)
	}
	if ch, ok := strings.CutSuffix(spec, ":latest"); ok {
		tag, err := m.LatestTag(ctx, ch)
		if err == nil {
			v, err := m.Install(ctx, ch, tag)
			if err == nil && v.SmokeOK {
				return v, nil
			}
			m.logf("yt-dlp: installing latest %s failed: %v", ch, err)
		}
		// Offline: fall back to the newest installed version of the channel.
		all, _ := m.Store.ListVersions(ctx)
		var best *store.YtdlpVersion
		for _, v := range all {
			if v.Channel == ch && v.SmokeOK && (best == nil || CompareTags(v.Tag, best.Tag) > 0) {
				best = v
			}
		}
		if best == nil {
			return nil, fmt.Errorf("no %s version available: %v", ch, err)
		}
		return best, nil
	}
	v, err := m.Store.GetVersion(ctx, spec)
	if errors.Is(err, store.ErrNotFound) {
		ch, tag, _ := strings.Cut(spec, "/")
		if config.ValidChannel(ch) {
			return m.Install(ctx, ch, tag)
		}
	}
	return v, err
}

// IsLatest reports whether a binary is the newest version of its channel
// (used to decide whether a "retry with latest nightly" can help).
func (m *Manager) IsLatest(ctx context.Context, b Binary) bool {
	tag, err := m.LatestTag(ctx, b.Channel)
	return err == nil && CompareTags(b.Tag, tag) >= 0
}

// Active returns the active version record.
func (m *Manager) Active(ctx context.Context) (*store.YtdlpVersion, error) {
	id := m.Settings.Settings().Ytdlp.Active
	if id == "" {
		return nil, store.ErrNotFound
	}
	return m.Store.GetVersion(ctx, id)
}

// Updates returns channels with a newer version than installed.
func (m *Manager) Updates() map[string]string {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.updates {
		out[k] = v
	}
	return out
}

// --- startup ---

// Startup registers the system/bundled builds and makes sure a version is
// active. It never blocks on the network: if nothing usable is installed,
// the latest version of the configured channel is installed in the
// background.
func (m *Manager) Startup(ctx context.Context) {
	m.init()
	if m.SystemPath != "" {
		m.registerSystem(ctx, m.SystemPath)
		return
	}
	m.registerBundled(ctx)
	st := m.Settings.Settings()
	if st.Ytdlp.Active != "" {
		if v, err := m.Store.GetVersion(ctx, st.Ytdlp.Active); err == nil {
			if _, err := os.Stat(v.Path); err == nil {
				return
			}
		}
		m.logf("yt-dlp: active version %s is missing", st.Ytdlp.Active)
	}
	if m.activateBest(ctx, st.Ytdlp.Channel) {
		return
	}
	// Development fallback: a yt-dlp on PATH.
	if p, err := exec.LookPath("yt-dlp"); err == nil {
		if v := m.registerSystem(ctx, p); v != nil {
			go m.installLatest(context.WithoutCancel(ctx), st.Ytdlp.Channel)
			return
		}
	}
	go m.installLatest(context.WithoutCancel(ctx), st.Ytdlp.Channel)
}

func (m *Manager) activateBest(ctx context.Context, channel string) bool {
	all, _ := m.Store.ListVersions(ctx)
	var best *store.YtdlpVersion
	for _, v := range all {
		if v.Channel != channel || !v.SmokeOK {
			continue
		}
		if _, err := os.Stat(v.Path); err != nil {
			continue
		}
		if best == nil || CompareTags(v.Tag, best.Tag) > 0 {
			best = v
		}
	}
	if best == nil {
		return false
	}
	return m.Activate(ctx, best.ID) == nil
}

func (m *Manager) installLatest(ctx context.Context, channel string) {
	tag, err := m.LatestTag(ctx, channel)
	if err != nil {
		m.logf("yt-dlp: cannot determine latest %s version: %v", channel, err)
		return
	}
	v, err := m.Install(ctx, channel, tag)
	if err != nil {
		m.logf("yt-dlp: install %s/%s failed: %v", channel, tag, err)
		return
	}
	if v.SmokeOK && m.Settings.Settings().Ytdlp.Channel == channel {
		cur, err := m.Active(ctx)
		if err != nil || cur.Channel == "system" || cur.Channel != channel || CompareTags(v.Tag, cur.Tag) > 0 {
			_ = m.Activate(ctx, v.ID)
		}
	}
}

func (m *Manager) registerSystem(ctx context.Context, path string) *store.YtdlpVersion {
	ver, err := ytdlp.Version(ctx, path)
	if err != nil {
		m.logf("yt-dlp: %s is not usable: %v", path, err)
		return nil
	}
	v := &store.YtdlpVersion{ID: "system/" + ver, Channel: "system", Tag: ver, Path: path, Dir: filepath.Dir(path)}
	m.smoke(ctx, v)
	if err := m.Store.UpsertVersion(ctx, v); err != nil {
		return nil
	}
	if m.SystemPath != "" || m.Settings.Settings().Ytdlp.Active == "" {
		if v.SmokeOK {
			_ = m.Activate(ctx, v.ID)
		}
	}
	return v
}

func (m *Manager) registerBundled(ctx context.Context) {
	if m.BundledDir == "" {
		return
	}
	for _, ch := range config.Channels {
		entries, err := os.ReadDir(filepath.Join(m.BundledDir, ch))
		if err != nil {
			continue
		}
		for _, e := range entries {
			dir := filepath.Join(m.BundledDir, ch, e.Name())
			man, err := ReadManifest(dir)
			if err != nil || man.Channel != ch || man.Tag != e.Name() {
				continue
			}
			id := versionID(ch, man.Tag)
			if old, err := m.Store.GetVersion(ctx, id); err == nil && old.SHA256 == man.SHA256 && old.SmokeOK {
				if _, err := os.Stat(old.Path); err == nil {
					continue // already registered and tested
				}
			}
			v := &store.YtdlpVersion{ID: id, Channel: ch, Tag: man.Tag, Dir: dir, Path: filepath.Join(dir, man.Exe),
				Asset: man.Asset, SHA256: man.SHA256, Bundled: true}
			m.smoke(ctx, v)
			if err := m.Store.UpsertVersion(ctx, v); err == nil {
				m.logf("yt-dlp: registered bundled %s (smoke test ok=%v)", id, v.SmokeOK)
			}
		}
	}
}

// --- auto-update ---

// CheckUpdates applies the per-channel auto-update policy (§6.2): "auto"
// installs the newest version and, for the active channel, activates it
// if the smoke test passes; "notify" only flags it.
func (m *Manager) CheckUpdates(ctx context.Context) {
	m.init()
	if m.SystemPath != "" {
		return
	}
	st := m.Settings.Settings()
	m.mu.Lock()
	m.lastCheck = time.Now()
	m.mu.Unlock()
	for _, ch := range config.Channels {
		policy := st.Ytdlp.AutoUpdate[ch]
		if policy == config.UpdateOff || policy == "" {
			continue
		}
		if policy == config.UpdateAuto && ch != st.Ytdlp.Channel {
			policy = config.UpdateNotify // only the active channel is auto-installed
		}
		tag, err := m.LatestTag(ctx, ch)
		if err != nil {
			continue
		}
		newest := ""
		all, _ := m.Store.ListVersions(ctx)
		for _, v := range all {
			if v.Channel == ch && (newest == "" || CompareTags(v.Tag, newest) > 0) {
				newest = v.Tag
			}
		}
		if newest != "" && CompareTags(newest, tag) >= 0 {
			m.mu.Lock()
			delete(m.updates, ch)
			m.mu.Unlock()
			continue
		}
		if policy == config.UpdateNotify || ch != st.Ytdlp.Channel {
			m.mu.Lock()
			m.updates[ch] = tag
			m.mu.Unlock()
			m.changed()
			continue
		}
		v, err := m.Install(ctx, ch, tag)
		if err != nil {
			m.logf("yt-dlp: auto-update to %s/%s failed: %v", ch, tag, err)
			continue
		}
		if !v.SmokeOK {
			m.logf("yt-dlp: auto-update %s/%s failed its smoke test, keeping the current version", ch, tag)
			m.mu.Lock()
			m.updates[ch] = tag
			m.mu.Unlock()
			continue
		}
		if err := m.Activate(ctx, v.ID); err != nil {
			m.logf("yt-dlp: activating %s failed: %v", v.ID, err)
		}
	}
}

// RunUpdater checks for updates shortly after start and then daily.
func (m *Manager) RunUpdater(ctx context.Context) {
	t := time.NewTimer(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.CheckUpdates(ctx)
		t.Reset(24 * time.Hour)
	}
}
