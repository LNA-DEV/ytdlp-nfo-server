// Package manager wires the store, scheduler, pipeline and version manager
// together and implements the operations the API exposes.
package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/buildinfo"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/events"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/joblog"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/notify"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/pipeline"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp/versions"
)

// Errors returned to the API.
var (
	ErrInvalid      = errors.New("invalid request")
	ErrShuttingDown = errors.New("server is shutting down")
	ErrConflict     = errors.New("conflict")
)

// DuplicateError is returned when an open job has the same URL.
type DuplicateError struct{ Job *store.Job }

func (e *DuplicateError) Error() string { return "a download already exists for this URL" }

// Options configure a Manager (tests override the external tools).
type Options struct {
	Env         *config.Env
	Resolver    pipeline.Resolver // nil: the real version manager
	HTTPClient  *http.Client
	TweakPolicy func(*scheduler.Policy)
	Logf        func(format string, args ...any)
	// DisableBackground turns off the auto-updater and pruning loops (tests).
	DisableBackground bool
	// Reddit points the Reddit client at a fake (tests): RootCAs, Dial and
	// Hosts are used.
	Reddit *browser.Options
}

// Manager is the application service.
type Manager struct {
	Env       *config.Env
	Store     *store.Store
	Settings  *SettingsService
	Hub       *events.Hub
	Logs      *joblog.Manager
	Versions  *versions.Manager
	Sched     *scheduler.Scheduler
	Pipe      *pipeline.Pipeline
	Finalizer *library.Finalizer
	Archive   *library.ArchiveFile
	Notifier  *notify.Notifier

	resolver   pipeline.Resolver
	logf       func(string, ...any)
	dataDir    string
	cookieDir  string
	tmpDataDir string
	opts       Options

	mu        sync.Mutex
	subs      subState
	reddit    redditState
	rules     []config.SiteRule
	live      map[int64]pipeline.Live
	warnings  map[string]Warning
	submitMu  sync.Mutex
	stopping  bool
	ctx       context.Context
	cancel    context.CancelFunc
	sysSignal chan struct{}
	bgWG      sync.WaitGroup
}

// Warning is a system-wide warning shown in the UI.
type Warning struct {
	Key     string    `json:"key"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// New opens the store, migrates v1 data and wires all components. Call
// Start to begin processing.
func New(o Options) (*Manager, error) {
	env := o.Env
	logf := o.Logf
	if logf == nil {
		logf = log.Printf
	}
	m := &Manager{Env: env, logf: logf, opts: o, live: map[int64]pipeline.Live{}, warnings: map[string]Warning{},
		sysSignal: make(chan struct{}, 1)}
	m.reddit.testOpts = o.Reddit

	m.dataDir = env.DataDir
	if m.dataDir == "" {
		tmp, err := os.MkdirTemp("", "ytdlp-nfo-server-")
		if err != nil {
			return nil, err
		}
		m.dataDir, m.tmpDataDir = tmp, tmp
	}
	for _, d := range []string{m.dataDir, env.DownloadDir, env.WorkRoot(), env.LibraryRoot()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	m.cookieDir = filepath.Join(m.dataDir, "cookies")
	if err := os.MkdirAll(m.cookieDir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(m.cookieDir, 0o700)

	dbPath := ""
	if env.DataDir != "" {
		dbPath = filepath.Join(env.DataDir, "ytdlp-nfo-server.db")
	}
	firstStart := dbPath == ""
	if dbPath != "" {
		if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
			firstStart = true
		}
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	m.Store = st

	logs, err := joblog.New(filepath.Join(m.dataDir, "logs"))
	if err != nil {
		return nil, err
	}
	m.Logs = logs
	m.Hub = events.NewHub(2048)
	m.Archive = &library.ArchiveFile{Path: env.LegacyArchivePath()}
	m.Finalizer = library.NewFinalizer()
	m.Notifier = &notify.Notifier{Client: o.HTTPClient, Logf: logf}

	if err := m.initSettings(firstStart); err != nil {
		return nil, err
	}
	if err := m.reloadRules(context.Background()); err != nil {
		return nil, err
	}
	if err := m.migrateV1(firstStart); err != nil {
		return nil, fmt.Errorf("migrate v1 data: %w", err)
	}

	m.Versions = &versions.Manager{
		Store: st, Settings: m.Settings,
		Fetcher:    &versions.Fetcher{Client: o.HTTPClient, Token: env.GitHubToken},
		Dir:        filepath.Join(m.dataDir, "ytdlp"),
		BundledDir: env.BundledYtdlDir,
		SystemPath: env.YtdlpPath,
		Logf:       logf,
		OnChange:   m.systemChanged,
	}
	m.resolver = o.Resolver
	if m.resolver == nil {
		m.resolver = m.Versions
	}

	s := m.Settings.Settings()
	// The scheduler calls the pipeline, the pipeline talks back to the
	// scheduler (cooldowns, pause); the closure breaks the cycle.
	m.Sched = scheduler.New(func(ctx context.Context, id int64) scheduler.Outcome {
		return m.Pipe.Run(ctx, id)
	}, s.MaxConcurrent, m.siteLimit, m.systemChanged)
	m.Pipe = pipeline.New(pipeline.Deps{
		Store: st, Env: env, Settings: m.Settings.Settings, SiteRules: m.siteRules,
		Versions: m.resolver, Logs: logs, Pub: m, Sched: m.Sched, Finalizer: m.Finalizer, Archive: m.Archive,
		CookieDir: m.cookieDir, CacheDir: filepath.Join(m.dataDir, "ytdlp", "cache"),
		ServerInfo: buildinfo.String(), TweakPolicy: o.TweakPolicy,
	})
	if err := m.initSubscriptions(); err != nil {
		return nil, err
	}
	return m, nil
}

// Start recovers from a crash, starts the scheduler and background loops.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx, m.cancel = context.WithCancel(ctx)
	if m.opts.Resolver == nil {
		m.Versions.Startup(m.ctx)
	}
	if err := m.recover(m.ctx); err != nil {
		return fmt.Errorf("recovery: %w", err)
	}
	m.Sched.Start(m.ctx)
	m.Sched.SetBackgroundSlots(m.Settings.Settings().Subscriptions.BackgroundSlots)
	if err := m.enqueueOpen(m.ctx); err != nil {
		return err
	}
	m.bgWG.Add(1)
	go m.systemLoop()
	if err := m.startSubscriptions(); err != nil {
		return err
	}
	if !m.opts.DisableBackground {
		m.bgWG.Add(2)
		go func() { defer m.bgWG.Done(); m.Versions.RunUpdater(m.ctx) }()
		go m.maintenanceLoop()
	}
	return nil
}

// Shutdown stops accepting work, interrupts running downloads (they resume
// after the next start) and closes the database.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	err := m.Sched.Shutdown(ctx)
	if m.cancel != nil {
		m.cancel()
	}
	m.bgWG.Wait()
	m.closeReddit()
	m.Hub.Close()
	if cerr := m.Store.Close(); err == nil {
		err = cerr
	}
	if m.tmpDataDir != "" {
		os.RemoveAll(m.tmpDataDir)
	}
	return err
}

func (m *Manager) shuttingDown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopping
}

// DataDir returns the effective data directory.
func (m *Manager) DataDir() string { return m.dataDir }

// CookieDir returns where cookie profiles are stored.
func (m *Manager) CookieDir() string { return m.cookieDir }

// --- site rules ---

func (m *Manager) reloadRules(ctx context.Context) error {
	rules, err := m.Store.ListSiteRules(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.rules = rules
	m.mu.Unlock()
	return nil
}

func (m *Manager) siteRules() []config.SiteRule {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]config.SiteRule(nil), m.rules...)
}

// siteGroup is the scheduler key for per-site limits and cooldowns.
func (m *Manager) siteGroup(url string, o config.Options) string {
	if o.SiteRuleID != 0 {
		for _, r := range m.siteRules() {
			if r.ID == o.SiteRuleID && r.MaxConcurrent > 0 {
				return fmt.Sprintf("rule:%d", r.ID)
			}
		}
	}
	return config.SiteKey(config.Host(url))
}

func (m *Manager) siteLimit(site string) int {
	var id int64
	if _, err := fmt.Sscanf(site, "rule:%d", &id); err != nil {
		return 0
	}
	for _, r := range m.siteRules() {
		if r.ID == id {
			return r.MaxConcurrent
		}
	}
	return 0
}

// --- background ---

func (m *Manager) maintenanceLoop() {
	defer m.bgWG.Done()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		m.prune(m.ctx)
		m.pruneGone(m.ctx)
		_ = m.Store.PruneSessions(m.ctx)
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pruneGone deletes subscription entries that are gone from their source
// and were never downloaded, after pruneGoneDays (off by default).
func (m *Manager) pruneGone(ctx context.Context) {
	days := m.Settings.Settings().Subscriptions.PruneGoneDays
	if days <= 0 {
		return
	}
	if n, err := m.Store.PruneGoneEntries(ctx, time.Now().AddDate(0, 0, -days)); err == nil && n > 0 {
		m.logf("subscriptions: pruned %d gone entries older than %d days", n, days)
		m.markCounts(0)
	}
}

// prune removes completed job records older than the configured age
// (off by default). Files are never touched.
func (m *Manager) prune(ctx context.Context) {
	days := m.Settings.Settings().AutoPruneDays
	if days <= 0 {
		return
	}
	ids, err := m.Store.PrunableJobIDs(ctx, time.Now().AddDate(0, 0, -days))
	if err != nil {
		return
	}
	for _, id := range ids {
		_ = m.Delete(ctx, id)
	}
	if len(ids) > 0 {
		m.logf("auto-prune: removed %d completed job records older than %d days", len(ids), days)
	}
}
