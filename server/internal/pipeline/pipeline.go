// Package pipeline runs a job: probe → plan → per item: fetch meta →
// select formats → download → verify → extras → NFO → finalize (§4.4).
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/artwork"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/failpoint"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/joblog"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp/versions"
)

// Resolver resolves yt-dlp binaries (implemented by versions.Manager).
type Resolver interface {
	Acquire(ctx context.Context, spec string) (versions.Binary, func(), error)
	IsLatest(ctx context.Context, b versions.Binary) bool
}

// Coordinator is the part of the scheduler the pipeline talks to.
type Coordinator interface {
	SetCooldown(site string, until time.Time)
	Pause(reason string, until time.Time)
}

// Live is the in-memory progress of a running job.
type Live struct {
	JobID      int64   `json:"jobId"`
	ItemID     int64   `json:"itemId"`
	ItemIndex  int     `json:"itemIndex"`
	ItemCount  int     `json:"itemCount"`
	ItemTitle  string  `json:"itemTitle"`
	Stage      string  `json:"stage"`
	Percent    float64 `json:"percent"`
	Speed      float64 `json:"speed"`
	ETA        float64 `json:"eta"`
	Downloaded float64 `json:"downloaded"`
	Total      float64 `json:"total"`
	Part       int     `json:"part"`
	Parts      int     `json:"parts"`
}

// Publisher receives state changes (implemented by the manager).
type Publisher interface {
	JobChanged(job *store.Job)
	ItemChanged(it *store.Item)
	Progress(l Live)
	JobFinished(job *store.Job)
	ItemFinalized(job *store.Job, it *store.Item, libraryRoot string)
	SystemWarning(key, msg string)
}

// Deps are the pipeline's collaborators.
type Deps struct {
	Store       *store.Store
	Env         *config.Env
	Settings    func() config.Settings
	SiteRules   func() []config.SiteRule
	Versions    Resolver
	Logs        *joblog.Manager
	Pub         Publisher
	Sched       Coordinator
	Finalizer   *library.Finalizer
	Archive     *library.ArchiveFile
	Artwork     *artwork.Maker
	CookieDir   string
	CacheDir    string
	ServerInfo  string // "v2.0.0 (commit …)"
	MetaMaxAge  time.Duration
	TweakPolicy func(*scheduler.Policy) // tests: shorter backoff
	Now         func() time.Time
}

// Pipeline runs jobs.
type Pipeline struct {
	d        Deps
	keys     keyLocks
	toolOnce sync.Once
	tools    string
}

// New creates a pipeline.
func New(d Deps) *Pipeline {
	if d.MetaMaxAge == 0 {
		d.MetaMaxAge = 30 * time.Minute
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Artwork == nil {
		d.Artwork = &artwork.Maker{FFmpeg: d.Env.FFmpegPath}
	}
	return &Pipeline{d: d, keys: keyLocks{m: map[string]chan struct{}{}}}
}

var bg = context.Background()

// runCtx holds the per-run state of a job.
type runCtx struct {
	job         *store.Job
	log         *joblog.Log
	bin         versions.Binary
	release     func()
	impersonate string
	cookies     string
	settings    config.Settings
	policy      scheduler.Policy
	itemCount   int
	lastPublish time.Time
	impWarned   bool
}

func (rc *runCtx) callOpts(p *Pipeline) ytdlp.CallOptions {
	return ytdlp.CallOptions{
		Impersonate:   rc.impersonate,
		CookiesFile:   rc.cookies,
		LimitRate:     rc.job.Options.LimitRate,
		SleepRequests: rc.job.Options.SleepRequests,
		SleepInterval: rc.job.Options.SleepInterval,
		CacheDir:      p.d.CacheDir,
	}
}

func (p *Pipeline) policy(s config.Settings) scheduler.Policy {
	pol := scheduler.DefaultPolicy()
	pol.MaxRetries = s.MaxRetries
	pol.RateLimitCooldown = time.Duration(s.RateLimitCooldownMinutes) * time.Minute
	pol.EscalateImpersonation = s.ImpersonationEscalation
	pol.NightlyOnBroken = s.Ytdlp.NightlyRetryOnBroken
	pol.Now = p.d.Now
	if p.d.TweakPolicy != nil {
		p.d.TweakPolicy(&pol)
	}
	return pol
}

func (p *Pipeline) saveJob(j *store.Job) {
	if err := p.d.Store.UpdateJob(bg, j); err != nil && !errors.Is(err, store.ErrNotFound) {
		p.d.Pub.SystemWarning("db", "Saving job failed: "+err.Error())
	}
	p.d.Pub.JobChanged(j)
}

func (p *Pipeline) saveItem(it *store.Item) {
	if err := p.d.Store.UpdateItem(bg, it); err != nil && !errors.Is(err, store.ErrNotFound) {
		p.d.Pub.SystemWarning("db", "Saving item failed: "+err.Error())
	}
	p.d.Pub.ItemChanged(it)
}

// JobWorkDir returns the work directory of a job.
func JobWorkDir(env *config.Env, jobID int64) string {
	return filepath.Join(env.WorkRoot(), strconv.FormatInt(jobID, 10))
}

// ItemWorkDir returns the work directory of an item.
func ItemWorkDir(env *config.Env, jobID, itemID int64) string {
	return filepath.Join(JobWorkDir(env, jobID), strconv.FormatInt(itemID, 10))
}

// Run runs a job. It is the scheduler's RunFunc.
func (p *Pipeline) Run(ctx context.Context, jobID int64) scheduler.Outcome {
	job, err := p.d.Store.GetJob(bg, jobID)
	if err != nil {
		return scheduler.Outcome{Kind: scheduler.Finished}
	}
	st := p.d.Settings()
	rc := &runCtx{job: job, log: p.d.Logs.Get(jobID), settings: st, policy: p.policy(st)}
	defer p.d.Logs.Release(jobID)
	defer func() {
		if rc.release != nil {
			rc.release()
		}
		os.Remove(filepath.Join(JobWorkDir(p.d.Env, jobID), ytdlp.CookieFile))
	}()

	firstRun := job.StartedAt.IsZero()
	if firstRun {
		job.StartedAt = p.d.Now()
	}
	job.RetryAt = time.Time{}
	if job.Options.Impersonate != "" {
		rc.impersonate = job.Options.Impersonate
	}
	if job.Retry.Impersonated && rc.impersonate == "" {
		rc.impersonate = "any"
	}

	if err := os.MkdirAll(JobWorkDir(p.d.Env, jobID), 0o755); err != nil {
		return p.failJob(rc, ytdlp.Classification{Class: ytdlp.ClassTransient, Reason: "cannot create work dir: " + err.Error()})
	}
	spec := job.Options.YtdlpVersion
	if job.Retry.NightlyTried && spec == "" {
		spec = config.ChannelNightly + ":latest"
	}
	bin, release, err := p.d.Versions.Acquire(ctx, spec)
	if err != nil {
		if ctx.Err() != nil {
			return p.stop(ctx, rc)
		}
		rc.log.Printf("--- No usable yt-dlp: %v ---", err)
		cls := ytdlp.Classification{Class: ytdlp.ClassTransient, Reason: "no usable yt-dlp version: " + err.Error(), Norm: "no ytdlp"}
		return p.jobFailure(ctx, rc, cls)
	}
	rc.bin, rc.release = bin, release
	job.YtdlpVersion = bin.ID
	if firstRun {
		p.header(rc)
	} else {
		rc.log.Printf("--- Resuming with yt-dlp %s (%s) ---", bin.Version, bin.ID)
	}

	if job.Options.CookieProfile != "" {
		path, err := p.prepareCookies(job)
		if err != nil {
			return p.failJob(rc, ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: "cookie profile: " + err.Error(),
				Hint: "Upload the cookie profile again or choose another one."})
		}
		rc.cookies = path
	}

	items, err := p.d.Store.ItemsForJob(bg, jobID)
	if err != nil {
		return p.failJob(rc, ytdlp.Classification{Class: ytdlp.ClassTransient, Reason: err.Error()})
	}
	if len(items) == 0 {
		job.State = store.JobProbing
		p.saveJob(job)
		out, ok := p.probe(ctx, rc)
		if !ok {
			return out
		}
	}
	job.State = store.JobRunning
	job.Error, job.ErrorClass, job.Hint = "", "", ""
	p.saveJob(job)
	return p.processItems(ctx, rc)
}

// header writes the version header every job log starts with (F23).
func (p *Pipeline) header(rc *runCtx) {
	p.toolOnce.Do(func() { p.tools = toolVersions(p.d.Env) })
	o := rc.job.Options
	imp := "unavailable"
	if rc.bin.ImpersonateAvailable {
		imp = fmt.Sprintf("available (%d targets)", len(rc.bin.Targets))
	}
	impOpt := o.Impersonate
	if impOpt == "" {
		impOpt = "off"
	}
	cookies := o.CookieProfile
	if cookies == "" {
		cookies = "none"
	}
	rc.log.Printf("=== ytdlp-nfo-server %s ===", p.d.ServerInfo)
	rc.log.Printf("yt-dlp %s (%s) at %s", rc.bin.Version, rc.bin.ID, rc.bin.Path)
	rc.log.Printf("%s", p.tools)
	rc.log.Printf("Impersonation: %s", imp)
	rc.log.Printf("Options: container=%s preset=%s allAudio=%v subtitles=%v impersonate=%s cookies=%s library=%s force=%v",
		o.Container, o.Preset, o.AllAudio, o.Subtitles, impOpt, cookies, o.Library, o.Force)
	rc.log.Printf("URL: %s", rc.job.URL)
}

// stop handles cancellation and shutdown.
func (p *Pipeline) stop(ctx context.Context, rc *runCtx) scheduler.Outcome {
	cause := context.Cause(ctx)
	job := rc.job
	items, _ := p.d.Store.ItemsForJob(bg, job.ID)
	switch {
	case errors.Is(cause, scheduler.ErrDeleted):
		return scheduler.Outcome{Kind: scheduler.Finished}
	case errors.Is(cause, scheduler.ErrCancelled):
		rc.log.Line("--- Cancelled ---")
		for _, it := range items {
			if !it.State.Terminal() && it.State != store.ItemFinalizing {
				it.State = store.ItemCancelled
				p.saveItem(it)
			}
		}
		job.State = store.JobCancelled
		job.FinishedAt = p.d.Now()
		job.Error, job.ErrorClass, job.Hint = "Cancelled", "", ""
		p.saveJob(job)
	default: // shutdown: resume after restart
		rc.log.Line("--- Interrupted by server shutdown; will resume after restart ---")
		for _, it := range items {
			if it.State.InProgress() && it.State != store.ItemFinalizing {
				it.State = store.ItemPending
				p.saveItem(it)
			}
		}
		job.State = store.JobQueued
		p.saveJob(job)
	}
	return scheduler.Outcome{Kind: scheduler.Finished}
}

// failJob marks the job failed without retry.
func (p *Pipeline) failJob(rc *runCtx, cls ytdlp.Classification) scheduler.Outcome {
	job := rc.job
	if cls.Hint == "" {
		cls.Hint = defaultHint(cls)
	}
	rc.log.Printf("--- Failed (%s: %s). Not retrying ---", cls.Class, cls.Reason)
	job.State = store.JobFailed
	job.Error, job.ErrorClass, job.Hint = cls.Reason, string(cls.Class), cls.Hint
	job.FinishedAt = p.d.Now()
	p.saveJob(job)
	p.d.Pub.JobFinished(job)
	return scheduler.Outcome{Kind: scheduler.Finished}
}

func defaultHint(c ytdlp.Classification) string {
	switch c.Class {
	case ytdlp.ClassPermanent:
		return ytdlp.HintPermanent
	case ytdlp.ClassExtractorBroken:
		return ytdlp.HintExtractorBroken
	case ytdlp.ClassAuthRequired:
		return ytdlp.HintAuthRequired
	case ytdlp.ClassBlocked:
		return ytdlp.HintBlocked
	case ytdlp.ClassCrash:
		return ytdlp.HintCrash
	}
	return c.Reason
}

// jobFailure applies the retry policy to a job-level (probe) failure.
func (p *Pipeline) jobFailure(ctx context.Context, rc *runCtx, cls ytdlp.Classification) scheduler.Outcome {
	job := rc.job
	job.Attempts++
	sit := scheduler.Situation{
		Class: cls, Failures: job.Attempts,
		ImpersonationAvailable: rc.bin.ImpersonateAvailable, Impersonating: rc.impersonate != "",
		NightlyCanHelp: p.nightlyCanHelp(ctx, rc),
	}
	d := rc.policy.Decide(&job.Retry, sit)
	at := p.d.Now().Add(d.Delay)
	rc.log.Line(scheduler.LogLine(d, sit, budget(rc.policy, cls), at))
	if !d.Retry {
		if strings.Contains(d.Why, "impersonation is unavailable") {
			cls.Hint = ytdlp.HintBlocked + " Impersonation is not available in this yt-dlp build."
		}
		return p.failJob(rc, cls)
	}
	if d.Cooldown > 0 {
		p.d.Sched.SetCooldown(job.Site, p.d.Now().Add(d.Cooldown))
		cls.Hint = fmt.Sprintf("Rate-limited by the site, resuming at %s", at.Format("15:04"))
	}
	job.State = store.JobRetrying
	job.Error, job.ErrorClass = cls.Reason, string(cls.Class)
	if cls.Hint == "" {
		cls.Hint = defaultHint(cls)
	}
	job.Hint = cls.Hint
	job.RetryAt = at
	p.saveJob(job)
	return scheduler.Outcome{Kind: scheduler.RetryAt, At: at}
}

func budget(pol scheduler.Policy, cls ytdlp.Classification) int {
	if cls.Class == ytdlp.ClassUnknown {
		return min(max(pol.UnknownBudget, 1), pol.MaxRetries)
	}
	return pol.MaxRetries
}

func (p *Pipeline) nightlyCanHelp(ctx context.Context, rc *runCtx) bool {
	if rc.bin.Channel != config.ChannelNightly {
		return true
	}
	return !p.d.Versions.IsLatest(ctx, rc.bin)
}

// escalate applies a policy escalation. It returns false if it failed.
func (p *Pipeline) escalate(ctx context.Context, rc *runCtx, how string) bool {
	switch how {
	case scheduler.EscalateImpersonate:
		rc.impersonate = "any"
		rc.log.Line(`--- Retrying with impersonation (--impersonate="") ---`)
	case scheduler.EscalateNightly:
		bin, release, err := p.d.Versions.Acquire(ctx, config.ChannelNightly+":latest")
		if err != nil {
			rc.log.Printf("--- Could not get the latest nightly: %v ---", err)
			return false
		}
		if rc.release != nil {
			rc.release()
		}
		rc.bin, rc.release = bin, release
		rc.job.YtdlpVersion = bin.ID
		rc.log.Printf("--- Retrying with yt-dlp %s (%s) ---", bin.Version, bin.ID)
	}
	return true
}

// probe enumerates the job's videos and creates its items.
func (p *Pipeline) probe(ctx context.Context, rc *runCtx) (scheduler.Outcome, bool) {
	job := rc.job
	for {
		rc.log.Printf("Probing %s", job.URL)
		res := ytdlp.Run(ctx, ytdlp.Call{
			Bin: rc.bin.Path, Args: ytdlp.ProbeArgs(job.URL, rc.callOpts(p)), CaptureStdout: true,
			OnLine: func(l string) { rc.log.Line(l) },
		})
		if ctx.Err() != nil {
			return p.stop(ctx, rc), false
		}
		p.noteImpersonation(rc, nil, res)
		var cls ytdlp.Classification
		if res.OK() {
			pr, err := ytdlp.ParseProbe(res.Stdout)
			if err == nil {
				if err := p.plan(ctx, rc, pr, 0); err != nil {
					if ctx.Err() != nil {
						return p.stop(ctx, rc), false
					}
					return p.failJob(rc, ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: err.Error()}), false
				}
				return scheduler.Outcome{}, true
			}
			cls = ytdlp.Classification{Class: ytdlp.ClassUnknown, Reason: err.Error(), Norm: "unparsable probe output"}
		} else {
			cls = ytdlp.Classify(res)
		}
		job.Attempts++
		sit := scheduler.Situation{
			Class: cls, Failures: job.Attempts,
			ImpersonationAvailable: rc.bin.ImpersonateAvailable, Impersonating: rc.impersonate != "",
			NightlyCanHelp: p.nightlyCanHelp(ctx, rc),
		}
		d := rc.policy.Decide(&job.Retry, sit)
		at := p.d.Now().Add(d.Delay)
		rc.log.Line(scheduler.LogLine(d, sit, budget(rc.policy, cls), at))
		if !d.Retry {
			if strings.Contains(d.Why, "impersonation is unavailable") {
				cls.Hint = ytdlp.HintBlocked + " Impersonation is not available in this yt-dlp build."
			}
			return p.failJob(rc, cls), false
		}
		if d.Escalate != "" {
			if !p.escalate(ctx, rc, d.Escalate) {
				return p.failJob(rc, cls), false
			}
			p.saveJob(job)
			continue
		}
		if d.Cooldown > 0 {
			p.d.Sched.SetCooldown(job.Site, p.d.Now().Add(d.Cooldown))
			cls.Hint = fmt.Sprintf("Rate-limited by the site, resuming at %s", at.Format("15:04"))
		}
		if cls.Hint == "" {
			cls.Hint = defaultHint(cls)
		}
		job.State = store.JobRetrying
		job.Error, job.ErrorClass, job.Hint = cls.Reason, string(cls.Class), cls.Hint
		job.RetryAt = at
		p.saveJob(job)
		return scheduler.Outcome{Kind: scheduler.RetryAt, At: at}, false
	}
}

// plan creates the items of a job from a probe result.
func (p *Pipeline) plan(ctx context.Context, rc *runCtx, pr *ytdlp.ProbeResult, depth int) error {
	job := rc.job
	entries, err := p.expandNested(ctx, rc, pr.Entries, depth)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("no videos found")
	}
	if pr.Title != "" {
		job.Title = pr.Title
	}
	if pr.Uploader != "" {
		job.Uploader = pr.Uploader
	}
	if pr.Thumbnail != "" {
		job.Thumbnail = pr.Thumbnail
	}
	job.IsPlaylist = pr.IsPlaylist
	playlistTitle := ""
	if pr.IsPlaylist {
		playlistTitle = job.Title
	}
	items, skipped := p.newItems(rc, entries, 0, playlistTitle)
	if err := p.d.Store.CreateItems(bg, items); err != nil {
		return err
	}
	if pr.Single != nil && len(items) == 1 && items[0].State == store.ItemPending {
		// The probe of a single video is its full metadata: reuse it.
		dir := ItemWorkDir(p.d.Env, job.ID, items[0].ID)
		if err := os.MkdirAll(dir, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(dir, ytdlp.InfoFile), pr.Single, 0o644)
		}
	}
	_ = failpoint.Check("pipeline:probed")
	if pr.IsPlaylist {
		rc.log.Printf("Found %d items in %q (%d already in library)", len(items), job.Title, skipped)
	} else if skipped > 0 {
		rc.log.Printf("Already in library: %s", items[0].Title)
	}
	p.saveJob(job)
	return nil
}

// newItems builds items for entries, marking archived ones as skipped.
func (p *Pipeline) newItems(rc *runCtx, entries []ytdlp.Entry, startIndex int, playlistTitle string) ([]*store.Item, int) {
	job := rc.job
	seen := map[string]bool{}
	var items []*store.Item
	skipped := 0
	for i, e := range entries {
		it := &store.Item{
			JobID: job.ID, Index: startIndex + i, URL: e.URL, Extractor: e.ExtractorKey, VideoID: e.ID,
			ArchiveKey: e.Key(), Title: e.Title, PlaylistTitle: playlistTitle, State: store.ItemPending,
		}
		if it.Title == "" {
			it.Title = e.ID
		}
		switch {
		case config.ValidateURL(e.URL) != nil:
			it.State = store.ItemFailed
			it.ErrorClass = string(ytdlp.ClassPermanent)
			it.Error = "entry has no usable URL"
			it.Hint = ytdlp.HintPermanent
			it.FinishedAt = p.d.Now()
		case it.ArchiveKey != "" && seen[it.ArchiveKey]:
			it.State = store.ItemSkipped
			it.Error = "Duplicate entry in this playlist"
			it.FinishedAt = p.d.Now()
			skipped++
		case it.ArchiveKey != "" && !job.Options.Force && p.inArchive(it.ArchiveKey):
			it.State = store.ItemSkipped
			it.Error = "Already in library"
			it.FinishedAt = p.d.Now()
			skipped++
		}
		if it.ArchiveKey != "" {
			seen[it.ArchiveKey] = true
		}
		items = append(items, it)
	}
	return items, skipped
}

func (p *Pipeline) inArchive(key string) bool {
	ok, _ := p.d.Store.HasArchive(bg, key)
	return ok
}

// expandNested probes entries that are themselves playlists (e.g. channel
// tabs), flattening them in Go (§4.4).
func (p *Pipeline) expandNested(ctx context.Context, rc *runCtx, entries []ytdlp.Entry, depth int) ([]ytdlp.Entry, error) {
	var out []ytdlp.Entry
	for _, e := range entries {
		if !e.Nested {
			out = append(out, e)
			continue
		}
		if depth >= 3 || config.ValidateURL(e.URL) != nil {
			rc.log.Printf("WARNING: skipping nested playlist %q (too deep or no URL)", e.Title)
			continue
		}
		rc.log.Printf("Listing nested playlist %s", e.URL)
		res := ytdlp.Run(ctx, ytdlp.Call{Bin: rc.bin.Path, Args: ytdlp.ProbeArgs(e.URL, rc.callOpts(p)), CaptureStdout: true,
			OnLine: func(l string) { rc.log.Line(l) }})
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !res.OK() {
			rc.log.Printf("WARNING: could not list %q: %s", e.Title, ytdlp.Classify(res).Reason)
			continue
		}
		sub, err := ytdlp.ParseProbe(res.Stdout)
		if err != nil {
			rc.log.Printf("WARNING: could not list %q: %v", e.Title, err)
			continue
		}
		nested, err := p.expandNested(ctx, rc, sub.Entries, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, nested...)
	}
	return out, nil
}

// processItems runs the items of a job sequentially.
func (p *Pipeline) processItems(ctx context.Context, rc *runCtx) scheduler.Outcome {
	job := rc.job
	for {
		if ctx.Err() != nil {
			return p.stop(ctx, rc)
		}
		items, err := p.d.Store.ItemsForJob(bg, job.ID)
		if err != nil {
			return p.failJob(rc, ytdlp.Classification{Class: ytdlp.ClassTransient, Reason: err.Error()})
		}
		rc.itemCount = len(items)
		now := p.d.Now()
		var next *store.Item
		var wait time.Time
		var waiting *store.Item
		for _, it := range items {
			if it.State.Terminal() {
				continue
			}
			if !it.RetryAt.IsZero() && it.RetryAt.After(now) {
				if wait.IsZero() || it.RetryAt.Before(wait) {
					wait, waiting = it.RetryAt, it
				}
				continue
			}
			next = it
			break
		}
		if next == nil {
			if !wait.IsZero() {
				job.State = store.JobRetrying
				job.RetryAt = wait
				job.Error, job.ErrorClass, job.Hint = waiting.Error, waiting.ErrorClass, waiting.Hint
				p.saveJob(job)
				rc.log.Printf("--- Waiting until %s for the next retry ---", wait.Format("15:04:05"))
				return scheduler.Outcome{Kind: scheduler.RetryAt, At: wait}
			}
			return p.finishJob(rc, items)
		}
		if job.State != store.JobRunning || job.Error != "" {
			job.State = store.JobRunning
			job.Error, job.ErrorClass, job.Hint, job.RetryAt = "", "", "", time.Time{}
			p.saveJob(job)
		}
		switch p.processItem(ctx, rc, next) {
		case resultStop:
			return p.stop(ctx, rc)
		case resultRequeue:
			job.State = store.JobQueued
			p.saveJob(job)
			return scheduler.Outcome{Kind: scheduler.Requeue}
		}
	}
}

// finishJob computes the terminal job state (§4.3).
func (p *Pipeline) finishJob(rc *runCtx, items []*store.Item) scheduler.Outcome {
	job := rc.job
	var done, skipped, failed, cancelled int
	var firstFailed *store.Item
	for _, it := range items {
		switch it.State {
		case store.ItemDone:
			done++
		case store.ItemSkipped:
			skipped++
		case store.ItemFailed:
			failed++
			if firstFailed == nil {
				firstFailed = it
			}
		case store.ItemCancelled:
			cancelled++
		}
	}
	job.FinishedAt = p.d.Now()
	job.RetryAt = time.Time{}
	switch {
	case failed == 0 && cancelled == 0:
		job.State = store.JobCompleted
		job.Error, job.ErrorClass, job.Hint = "", "", ""
		if done == 0 && skipped > 0 {
			if skipped == 1 {
				job.Hint = "Already in library"
			} else {
				job.Hint = fmt.Sprintf("All %d items already in library", skipped)
			}
		}
	case done+skipped > 0:
		job.State = store.JobPartial
		job.Error = fmt.Sprintf("%d of %d items failed", failed+cancelled, len(items))
		if firstFailed != nil {
			job.ErrorClass, job.Hint = firstFailed.ErrorClass, firstFailed.Hint
		}
	default:
		job.State = store.JobFailed
		if firstFailed != nil {
			job.Error, job.ErrorClass, job.Hint = firstFailed.Error, firstFailed.ErrorClass, firstFailed.Hint
		} else {
			job.Error = "Cancelled"
		}
	}
	msg := fmt.Sprintf("=== Job %s: %d done, %d already in library, %d failed ===", job.State, done, skipped, failed)
	if job.Hint != "" && job.State == store.JobCompleted {
		msg = fmt.Sprintf("=== Job completed: %s ===", job.Hint)
	}
	rc.log.Line(msg)
	p.saveJob(job)
	if job.State == store.JobCompleted {
		os.RemoveAll(JobWorkDir(p.d.Env, job.ID))
	}
	p.d.Pub.JobFinished(job)
	return scheduler.Outcome{Kind: scheduler.Finished}
}

func (p *Pipeline) noteImpersonation(rc *runCtx, it *store.Item, res *ytdlp.Result) {
	if !ytdlp.ImpersonationUnavailable(res.Warnings) && !ytdlp.ImpersonationUnavailable(res.Tail) {
		return
	}
	if it != nil {
		it.AddWarning("Impersonation unavailable")
	}
	if !rc.impWarned {
		rc.impWarned = true
		rc.log.Line("WARNING: the site requested impersonation, but no impersonate target is available in this yt-dlp build")
		p.d.Pub.SystemWarning("impersonation", "An extractor requested impersonation, but no impersonate target is available.")
	}
}

// prepareCookies copies the job's cookie profile into its work dir: yt-dlp
// writes the jar back, so concurrent jobs must not share a file (§7.3).
func (p *Pipeline) prepareCookies(job *store.Job) (string, error) {
	prof, err := p.d.Store.GetCookieProfile(bg, job.Options.CookieProfile)
	if err != nil {
		return "", fmt.Errorf("profile %q not found", job.Options.CookieProfile)
	}
	src := filepath.Join(p.d.CookieDir, filepath.Base(prof.Filename))
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(JobWorkDir(p.d.Env, job.ID), ytdlp.CookieFile)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return "", err
	}
	return dst, os.Chmod(dst, 0o600)
}

// keyLocks makes sure the same video is never processed by two jobs at the
// same time (F20); the second one waits and then finds it in the archive.
type keyLocks struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func (k *keyLocks) lock(ctx context.Context, key string, onWait func()) (func(), error) {
	if key == "" {
		return func() {}, nil
	}
	waited := false
	for {
		k.mu.Lock()
		ch, held := k.m[key]
		if !held {
			ch = make(chan struct{})
			k.m[key] = ch
			k.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					k.mu.Lock()
					delete(k.m, key)
					close(ch)
					k.mu.Unlock()
				})
			}, nil
		}
		k.mu.Unlock()
		if !waited && onWait != nil {
			onWait()
			waited = true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
