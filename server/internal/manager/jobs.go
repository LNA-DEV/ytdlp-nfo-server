package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/pipeline"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// Submit creates a job. It returns *DuplicateError if an open job has the
// same (normalized) URL, so the legacy API can answer 409.
func (m *Manager) Submit(ctx context.Context, rawURL string, req config.OptionsRequest) (*store.Job, error) {
	res := m.SubmitBulk(ctx, []string{rawURL}, req)
	if len(res) == 0 {
		return nil, fmt.Errorf("%w: url is required", ErrInvalid)
	}
	r := res[0]
	if r.Dup != nil {
		return nil, &DuplicateError{Job: r.Dup}
	}
	if r.Err != nil {
		return nil, r.Err
	}
	return r.Job, nil
}

// BulkResult is the outcome for one submitted URL.
type BulkResult struct {
	URL string
	Job *store.Job
	Dup *store.Job
	Err error
}

// SubmitBulk creates jobs for many URLs at once (max 500 enforced by API).
func (m *Manager) SubmitBulk(ctx context.Context, urls []string, req config.OptionsRequest) []BulkResult {
	m.submitMu.Lock()
	defer m.submitMu.Unlock()
	settings := m.Settings.Settings()
	rules := m.siteRules()
	var results []BulkResult
	var jobs []*store.Job
	seen := map[string]bool{}
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		r := BulkResult{URL: u}
		if m.shuttingDown() {
			r.Err = ErrShuttingDown
			results = append(results, r)
			continue
		}
		if err := config.ValidateURL(u); err != nil {
			r.Err = fmt.Errorf("%w: %v", ErrInvalid, err)
			results = append(results, r)
			continue
		}
		norm := config.NormalizeURL(u)
		if seen[norm] {
			r.Dup = &store.Job{URL: u}
			results = append(results, r)
			continue
		}
		if existing, err := m.Store.FindOpenJobByNormURL(ctx, norm); err == nil {
			r.Dup = existing
			results = append(results, r)
			continue
		}
		opts, err := config.Resolve(req, &settings, rules, u)
		if err != nil {
			r.Err = fmt.Errorf("%w: %v", ErrInvalid, err)
			results = append(results, r)
			continue
		}
		if opts.CookieProfile != "" {
			if _, err := m.Store.GetCookieProfile(ctx, opts.CookieProfile); err != nil {
				r.Err = fmt.Errorf("%w: unknown cookie profile %q", ErrInvalid, opts.CookieProfile)
				results = append(results, r)
				continue
			}
		}
		seen[norm] = true
		j := &store.Job{URL: u, NormURL: norm, Site: m.siteGroup(u, opts), State: store.JobQueued, Requested: req, Options: opts}
		jobs = append(jobs, j)
		r.Job = j
		results = append(results, r)
	}
	if len(jobs) > 0 {
		if err := m.Store.CreateJobs(ctx, jobs); err != nil {
			for i := range results {
				if results[i].Job != nil {
					results[i].Job, results[i].Err = nil, err
				}
			}
			return results
		}
		for _, j := range jobs {
			m.Logs.Get(j.ID).Printf("Job created for %s", j.URL)
			m.Sched.Enqueue(scheduler.Job{ID: j.ID, Pos: j.QueuePos, Site: j.Site})
			m.publishJob(j.ID)
		}
		for i := range results {
			if results[i].Job != nil {
				if full, err := m.Store.GetJob(ctx, results[i].Job.ID); err == nil {
					results[i].Job = full
				}
			}
		}
	}
	return results
}

// ListFilter selects jobs.
type ListFilter struct {
	Tab    string // active, queued, completed, failed or "" (all)
	Query  string
	Limit  int
	Offset int
}

// List returns jobs for a tab with live progress.
func (m *Manager) List(ctx context.Context, f ListFilter) ([]JobView, error) {
	jobs, err := m.Store.ListJobs(ctx, store.JobFilter{States: store.TabStates(f.Tab), Query: f.Query, Limit: f.Limit, Offset: f.Offset})
	if err != nil {
		return nil, err
	}
	out := make([]JobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, m.view(j))
	}
	return out, nil
}

// Counts returns jobs per tab.
func (m *Manager) Counts(ctx context.Context) (map[string]int, error) {
	return m.Store.CountJobsByTab(ctx)
}

// Get returns a job with its items.
func (m *Manager) Get(ctx context.Context, id int64) (*JobView, error) {
	j, err := m.Store.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	v := m.view(j)
	items, err := m.Store.ItemsForJob(ctx, id)
	if err != nil {
		return nil, err
	}
	v.Items = make([]ItemView, 0, len(items))
	for _, it := range items {
		v.Items = append(v.Items, itemView(it))
	}
	return &v, nil
}

// GetJob returns the raw job.
func (m *Manager) GetJob(ctx context.Context, id int64) (*store.Job, error) {
	return m.Store.GetJob(ctx, id)
}

// RetryRequest describes a manual retry.
type RetryRequest struct {
	Options    *config.OptionsRequest `json:"options,omitempty"`
	FailedOnly *bool                  `json:"failedOnly,omitempty"`
}

// Retry re-queues a finished job: failed and cancelled items are reset
// (completed ones are kept), optionally with changed options. With
// failedOnly=false every item is downloaded again (force re-download).
func (m *Manager) Retry(ctx context.Context, id int64, rr RetryRequest) (*store.Job, error) {
	if m.shuttingDown() {
		return nil, ErrShuttingDown
	}
	m.submitMu.Lock()
	defer m.submitMu.Unlock()
	job, err := m.Store.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if !job.State.Terminal() {
		return nil, fmt.Errorf("%w: job is %s", ErrConflict, job.State)
	}
	failedOnly := rr.FailedOnly == nil || *rr.FailedOnly
	if rr.Options != nil {
		job.Requested = job.Requested.Merge(*rr.Options)
	}
	if !failedOnly {
		t := true
		job.Requested.Force = &t
	}
	settings := m.Settings.Settings()
	opts, err := config.Resolve(job.Requested, &settings, m.siteRules(), job.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if opts.CookieProfile != "" {
		if _, err := m.Store.GetCookieProfile(ctx, opts.CookieProfile); err != nil {
			return nil, fmt.Errorf("%w: unknown cookie profile %q", ErrInvalid, opts.CookieProfile)
		}
	}
	if job.State == store.JobCompleted && failedOnly {
		return nil, fmt.Errorf("%w: nothing to retry; use a forced re-download", ErrConflict)
	}
	items, err := m.Store.ItemsForJob(ctx, id)
	if err != nil {
		return nil, err
	}
	root, _ := settings.LibraryPath(m.Env, opts.Library)
	reset := 0
	for _, it := range items {
		retryItem := it.State == store.ItemFailed || it.State == store.ItemCancelled || it.State == store.ItemPending
		if !failedOnly && (it.State == store.ItemDone || it.State == store.ItemSkipped) {
			retryItem = !strings.HasPrefix(it.Error, "Playlist expanded") && !strings.HasPrefix(it.Error, "Duplicate entry")
		}
		if !retryItem {
			continue
		}
		reset++
		it.Attempts = 0
		it.Retry = store.RetryState{}
		it.RetryAt = time.Time{}
		it.Error, it.ErrorClass, it.Hint = "", "", ""
		it.Warnings = nil
		it.FinishedAt = time.Time{}
		lib := it.Library
		if lib == "" {
			lib = opts.Library
		}
		itemRoot, ok := settings.LibraryPath(m.Env, lib)
		if !ok {
			itemRoot = root
		}
		if it.FinalPath != "" && len(it.Files) > 0 && library.HasStaged(itemRoot, pipeline.UID(it.JobID, it.ID)) && it.State != store.ItemDone {
			it.State = store.ItemFinalizing // finish the interrupted move
		} else {
			it.State = store.ItemPending
			it.FinalPath, it.Files = "", nil
		}
		if err := m.Store.UpdateItem(ctx, it); err != nil {
			return nil, err
		}
	}
	job.Options = opts
	job.Site = m.siteGroup(job.URL, opts)
	job.State = store.JobQueued
	job.Attempts = 0
	job.Retry = store.RetryState{}
	job.RetryAt = time.Time{}
	job.Error, job.ErrorClass, job.Hint = "", "", ""
	job.FinishedAt = time.Time{}
	job.StartedAt = time.Time{} // the next run logs a full header with the new options
	if job.QueuePos, err = m.Store.NextQueuePos(ctx); err != nil {
		return nil, err
	}
	if err := m.Store.UpdateJob(ctx, job); err != nil {
		return nil, err
	}
	desc := "failed items"
	if !failedOnly {
		desc = "all items (forced re-download)"
	}
	if len(items) == 0 {
		desc = "probe"
	}
	m.Logs.Get(id).Printf("=== Retry requested: %s (%d reset) ===", desc, reset)
	m.Sched.Enqueue(scheduler.Job{ID: job.ID, Pos: job.QueuePos, Site: job.Site})
	m.publishJob(job.ID)
	return m.Store.GetJob(ctx, id)
}

// Cancel stops a queued or running job and keeps its record.
func (m *Manager) Cancel(ctx context.Context, id int64) (*store.Job, error) {
	job, err := m.Store.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if job.State.Terminal() {
		return nil, fmt.Errorf("%w: job is already %s", ErrConflict, job.State)
	}
	<-m.Sched.Cancel(id, scheduler.ErrCancelled)
	job, err = m.Store.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if !job.State.Terminal() {
		// It was waiting in the queue: no run to record the cancellation.
		items, _ := m.Store.ItemsForJob(ctx, id)
		for _, it := range items {
			if !it.State.Terminal() && it.State != store.ItemFinalizing {
				it.State = store.ItemCancelled
				_ = m.Store.UpdateItem(ctx, it)
			}
		}
		job.State = store.JobCancelled
		job.FinishedAt = time.Now()
		job.RetryAt = time.Time{}
		job.Error = "Cancelled"
		if err := m.Store.UpdateJob(ctx, job); err != nil {
			return nil, err
		}
		m.Logs.Get(id).Line("--- Cancelled ---")
	}
	m.removeWorkDirs(ctx, id, false)
	m.publishJob(id)
	return m.Store.GetJob(ctx, id)
}

// removeWorkDirs deletes work dirs of a job's unfinished items. Items with
// a pending finalize keep theirs unless all is set.
func (m *Manager) removeWorkDirs(ctx context.Context, jobID int64, all bool) {
	if all {
		os.RemoveAll(pipeline.JobWorkDir(m.Env, jobID))
		return
	}
	items, _ := m.Store.ItemsForJob(ctx, jobID)
	for _, it := range items {
		if it.State != store.ItemFinalizing {
			os.RemoveAll(pipeline.ItemWorkDir(m.Env, jobID, it.ID))
		}
	}
}

// Delete removes a job record, cancelling it first if it runs. Files in the
// library are never touched.
func (m *Manager) Delete(ctx context.Context, id int64) error {
	if _, err := m.Store.GetJob(ctx, id); err != nil {
		return err
	}
	// Wait until the process group is gone before deleting files (F9).
	<-m.Sched.Cancel(id, scheduler.ErrDeleted)
	if err := m.Store.DeleteJob(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	m.removeWorkDirs(ctx, id, true)
	m.cleanupStaging(ctx)
	m.Logs.Delete(id)
	m.mu.Lock()
	delete(m.live, id)
	m.mu.Unlock()
	m.Hub.Publish(EventJobRemoved, map[string]string{"id": itoa(id)})
	m.publishCounts()
	return nil
}

// cleanupStaging removes staging entries of deleted items.
func (m *Manager) cleanupStaging(ctx context.Context) {
	unfinished, err := m.Store.ItemsInStates(ctx, store.ItemFinalizing, store.ItemFailed, store.ItemPending, store.ItemCancelled)
	if err != nil {
		return
	}
	ref := map[string]bool{}
	for _, it := range unfinished {
		if it.FinalPath != "" {
			ref[pipeline.UID(it.JobID, it.ID)] = true
		}
	}
	for _, root := range m.libraryRoots() {
		m.Finalizer.CleanupUnreferenced(root, func(uid string) bool { return ref[uid] }, m.logf)
	}
}

// Clear removes the records of all jobs in a tab ("completed", "failed").
func (m *Manager) Clear(ctx context.Context, tab string) (int, error) {
	states := store.TabStates(tab)
	if tab != "completed" && tab != "failed" {
		return 0, fmt.Errorf("%w: only completed or failed jobs can be cleared", ErrInvalid)
	}
	ids, err := m.Store.JobIDs(ctx, states...)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := m.Delete(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return 0, err
		}
	}
	return len(ids), nil
}

// DeleteAll removes every job (legacy DELETE /api/jobs).
func (m *Manager) DeleteAll(ctx context.Context) error {
	ids, err := m.Store.JobIDs(ctx)
	if err != nil {
		return err
	}
	// Cancel everything first, in parallel, then delete.
	var waits []<-chan struct{}
	for _, id := range ids {
		waits = append(waits, m.Sched.Cancel(id, scheduler.ErrDeleted))
	}
	for _, w := range waits {
		<-w
	}
	for _, id := range ids {
		if err := m.Delete(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// Reorder puts the given queued jobs at the front, in this order.
func (m *Manager) Reorder(ctx context.Context, ids []int64) error {
	var valid []int64
	for _, id := range ids {
		j, err := m.Store.GetJob(ctx, id)
		if err != nil || j.State != store.JobQueued && j.State != store.JobRetrying {
			continue
		}
		valid = append(valid, id)
	}
	if len(valid) == 0 {
		return nil
	}
	if err := m.Store.SetQueuePositions(ctx, valid); err != nil {
		return err
	}
	pos := map[int64]int64{}
	for _, id := range valid {
		if j, err := m.Store.GetJob(ctx, id); err == nil {
			pos[id] = j.QueuePos
			m.publishJob(id)
		}
	}
	m.Sched.SetPositions(pos)
	return nil
}

// StartNow starts a queued job immediately, ignoring the limits.
func (m *Manager) StartNow(ctx context.Context, id int64) error {
	j, err := m.Store.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if j.State != store.JobQueued && j.State != store.JobRetrying {
		return fmt.Errorf("%w: job is %s", ErrConflict, j.State)
	}
	if !m.Sched.StartNow(id) {
		return fmt.Errorf("%w: job is not waiting in the queue", ErrConflict)
	}
	return nil
}

// RerunExtras re-fetches thumbnail and subtitles of a finished item.
func (m *Manager) RerunExtras(ctx context.Context, jobID, itemID int64) error {
	return m.Pipe.RerunExtras(ctx, jobID, itemID)
}
