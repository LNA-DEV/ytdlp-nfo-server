package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/pipeline"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// isUpgrade reports whether v1 data exists (first start of an upgraded
// install): such installs keep their naming and yt-dlp channel (§16).
func (m *Manager) isUpgrade() bool {
	if m.Env.DataDir != "" && fileExists(filepath.Join(m.Env.DataDir, "jobs.json")) {
		return true
	}
	return fileExists(m.Env.LegacyArchivePath())
}

func (m *Manager) initSettings(firstStart bool) error {
	ctx := context.Background()
	st, ok, err := m.Store.LoadSettings(ctx)
	if err != nil {
		return err
	}
	if !ok {
		migrated := m.isUpgrade()
		st = config.DefaultSettings(migrated)
		if migrated {
			m.logf("migration: v1 data found, keeping the legacy library layout and the %s yt-dlp channel", st.Ytdlp.Channel)
		}
		if err := m.Store.SaveSettings(ctx, st); err != nil {
			return err
		}
		_ = firstStart
	}
	st.Normalize()
	st.ApplyEnv(m.Env)
	if err := st.Validate(); err != nil {
		m.logf("settings: stored settings are invalid (%v), using defaults for invalid fields", err)
		d := config.DefaultSettings(m.isUpgrade())
		d.ApplyEnv(m.Env)
		st = d
	}
	m.Settings = &SettingsService{cur: st, st: m.Store, env: m.Env, onChange: m.settingsChanged}
	return nil
}

func (m *Manager) settingsChanged(old, cur config.Settings) {
	if old.MaxConcurrent != cur.MaxConcurrent && m.Sched != nil {
		m.Sched.SetMax(cur.MaxConcurrent)
	}
	if old.Subscriptions.BackgroundSlots != cur.Subscriptions.BackgroundSlots && m.Sched != nil {
		m.Sched.SetBackgroundSlots(cur.Subscriptions.BackgroundSlots)
	}
	if old.Subscriptions != cur.Subscriptions && m.subs.loop != nil {
		m.subs.loop.Wake() // e.g. resumed after "Pause all"
	}
	m.systemChanged()
}

// migrateV1 imports jobs.json and .ytdlp-archive.txt (§10). It is
// idempotent and keeps backup copies of the old files.
func (m *Manager) migrateV1(firstStart bool) error {
	ctx := context.Background()
	if m.Env.DataDir != "" {
		path := filepath.Join(m.Env.DataDir, "jobs.json")
		done, _ := m.Store.GetMeta(ctx, "v1_jobs_imported")
		if done == "" && fileExists(path) {
			if err := m.importJobsJSON(ctx, path); err != nil {
				return err
			}
			if err := m.Store.SetMeta(ctx, "v1_jobs_imported", "1"); err != nil {
				return err
			}
		}
	}
	// The archive file is shared with other tools: import new lines on
	// every start (existing keys are ignored).
	arch := m.Env.LegacyArchivePath()
	if fileExists(arch) {
		if done, _ := m.Store.GetMeta(ctx, "v1_archive_backup"); done == "" {
			backup := filepath.Join(m.dataDir, "ytdlp-archive.v1-backup.txt")
			if !fileExists(backup) {
				if err := copyFile(arch, backup); err != nil {
					m.logf("migration: backup of %s failed: %v", arch, err)
				}
			}
			_ = m.Store.SetMeta(ctx, "v1_archive_backup", "1")
		}
		keys, err := store.ReadArchiveFile(arch)
		if err != nil {
			return fmt.Errorf("read %s: %w", arch, err)
		}
		n, err := m.Store.ImportArchiveKeys(ctx, keys, "import")
		if err != nil {
			return err
		}
		if n > 0 {
			m.logf("archive: imported %d entries from %s", n, arch)
		}
	}
	_ = firstStart
	return nil
}

func (m *Manager) importJobsJSON(ctx context.Context, path string) error {
	backup := path + ".v1-backup"
	if !fileExists(backup) {
		if err := copyFile(path, backup); err != nil {
			return fmt.Errorf("backup %s: %w", path, err)
		}
	}
	state, err := store.ReadV1State(path)
	if err != nil {
		return err
	}
	settings := m.Settings.Settings()
	rules := m.siteRules()
	var jobs []*store.Job
	var maxID int64
	for _, v := range state.Jobs {
		j, err := v.ToJob()
		if err != nil {
			m.logf("migration: skipping job: %v", err)
			continue
		}
		if existing, err := m.Store.GetJob(ctx, j.ID); err == nil && existing != nil {
			continue // already imported
		}
		opts, err := config.Resolve(j.Requested, &settings, rules, j.URL)
		if err != nil {
			opts, _ = config.Resolve(config.OptionsRequest{}, &settings, rules, j.URL)
		}
		j.Options = opts
		j.Site = m.siteGroup(j.URL, opts)
		if config.ValidateURL(j.URL) != nil && !j.State.Terminal() {
			j.State = store.JobFailed
			j.Error = "invalid URL"
			j.ErrorClass = "permanent"
			j.FinishedAt = j.CreatedAt
		}
		jobs = append(jobs, j)
		maxID = max(maxID, j.ID)
	}
	if err := m.Store.CreateJobs(ctx, jobs); err != nil {
		return err
	}
	next := max(int64(state.NextID), maxID)
	// v1 guarded against a stale nextId by scanning numbered job dirs.
	if entries, err := os.ReadDir(m.Env.DownloadDir); err == nil {
		for _, e := range entries {
			if n, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && e.IsDir() && n > next {
				next = n
			}
		}
	}
	if err := m.Store.SetNextJobID(ctx, next); err != nil {
		return err
	}
	queued := 0
	for _, j := range jobs {
		if j.State == store.JobQueued {
			queued++
		}
	}
	m.logf("migration: imported %d jobs from %s (%d re-queued), backup at %s", len(jobs), path, queued, backup)
	return nil
}

// recover repairs state after a crash or kill -9 (§4.5).
func (m *Manager) recover(ctx context.Context) error {
	items, err := m.Store.ItemsInStates(ctx, store.ItemFetchingMeta, store.ItemDownloading, store.ItemVerifying, store.ItemExtras)
	if err != nil {
		return err
	}
	for _, it := range items {
		// The work dir is kept; yt-dlp resumes .part files.
		it.State = store.ItemPending
		if err := m.Store.UpdateItem(ctx, it); err != nil {
			return err
		}
	}
	jobs, err := m.Store.JobsInStates(ctx, store.JobProbing, store.JobRunning)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		j.State = store.JobQueued
		if err := m.Store.UpdateJob(ctx, j); err != nil {
			return err
		}
	}
	if n := len(items) + len(jobs); n > 0 {
		m.logf("recovery: re-queued %d interrupted jobs (%d items resume)", len(jobs), len(items))
	}

	m.syncLegacyArchive(ctx)

	// Staging/trash leftovers without a DB reference are removed (or
	// restored, for displaced library folders).
	referenced := map[string]bool{}
	unfinished, err := m.Store.ItemsInStates(ctx, store.ItemFinalizing, store.ItemFailed, store.ItemPending, store.ItemCancelled)
	if err != nil {
		return err
	}
	for _, it := range unfinished {
		if it.FinalPath != "" {
			referenced[pipeline.UID(it.JobID, it.ID)] = true
		}
	}
	for _, root := range m.libraryRoots() {
		m.Finalizer.CleanupUnreferenced(root, func(uid string) bool { return referenced[uid] }, m.logf)
	}

	// Work dirs of jobs that no longer exist or are complete.
	if entries, err := os.ReadDir(m.Env.WorkRoot()); err == nil {
		for _, e := range entries {
			id, err := strconv.ParseInt(e.Name(), 10, 64)
			if err != nil || !e.IsDir() {
				continue
			}
			j, err := m.Store.GetJob(ctx, id)
			if errors.Is(err, store.ErrNotFound) || err == nil && j.State == store.JobCompleted {
				m.logf("recovery: removing stale work dir %s", e.Name())
				os.RemoveAll(filepath.Join(m.Env.WorkRoot(), e.Name()))
			}
		}
	}
	return nil
}

func (m *Manager) libraryRoots() []string {
	s := m.Settings.Settings()
	roots := []string{m.Env.LibraryRoot()}
	for _, l := range s.Libraries {
		roots = append(roots, l.Path)
	}
	return roots
}

// enqueueOpen hands queued and waiting jobs to the scheduler.
func (m *Manager) enqueueOpen(ctx context.Context) error {
	jobs, err := m.Store.JobsInStates(ctx, store.JobQueued, store.JobRetrying)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		sj := scheduler.Job{ID: j.ID, Pos: j.QueuePos, Site: j.Site, Band: j.Band}
		if j.State == store.JobRetrying {
			sj.NotBefore = j.RetryAt
		}
		m.Sched.Enqueue(sj)
	}
	return nil
}

// LibraryUID returns the staging uid of an item (exported for tests).
func LibraryUID(jobID, itemID int64) string { return pipeline.UID(jobID, itemID) }

// syncLegacyArchive appends archive entries that are missing from the
// shared .ytdlp-archive.txt (e.g. after a crash between the DB record and
// the file append). The database is the source of truth.
func (m *Manager) syncLegacyArchive(ctx context.Context) {
	keys, err := m.Store.AllArchiveKeys(ctx)
	if err != nil || len(keys) == 0 {
		return
	}
	have := map[string]bool{}
	if fileKeys, err := store.ReadArchiveFile(m.Archive.Path); err == nil {
		for _, k := range fileKeys {
			have[k] = true
		}
	}
	added := 0
	for _, k := range keys {
		if !have[k] {
			if err := m.Archive.Append(k); err != nil {
				m.logf("archive: could not update %s: %v", m.Archive.Path, err)
				return
			}
			added++
		}
	}
	if added > 0 {
		m.logf("archive: added %d missing entries to %s", added, m.Archive.Path)
	}
}
