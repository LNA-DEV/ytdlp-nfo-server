package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/failpoint"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/joblog"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp/versions"
)

// Listing timeouts (Appendix A); a timeout is transient. A full listing of
// a source with many thousand entries on a site with small pages takes a
// while, and without it the source never gets its first listing.
const (
	WindowTimeout  = 5 * time.Minute
	FullTimeout    = 2 * time.Hour
	PreviewTimeout = 60 * time.Second
	// maxWindow: a gap larger than this is resolved by a full listing.
	maxWindow = 3200
)

// Host is what the check loop needs from the manager.
type Host interface {
	Settings() config.Settings
	// Resolve returns the resolved download options of a subscription and
	// the scheduler site key of its sources (§5.2 "options first").
	Resolve(ctx context.Context, sub *store.Subscription) (config.Options, string, error)
	Acquire(ctx context.Context, spec string) (versions.Binary, func(), error)
	IsLatest(ctx context.Context, b versions.Binary) bool
	// CopyCookies copies a cookie profile to path (yt-dlp writes the jar
	// back, so every check gets its own copy).
	CopyCookies(profile, path string) error
	Cooldown(site string) time.Time
	SetCooldown(site string, until time.Time)
	// Commit applies a check result in one transaction (§5.5).
	Commit(ctx context.Context, res *Result) error
	// Changed publishes the state of a subscription.
	Changed(id int64)
}

// Deps are the collaborators of the check loop.
type Deps struct {
	Store    *store.Store
	Host     Host
	Logs     *joblog.Manager // DATA_DIR/logs/subscriptions/<id>.log
	CacheDir string
	TmpDir   string
	Logf     func(format string, args ...any)
	Now      func() time.Time
}

type doneMsg struct {
	id   int64
	site string
}

// Loop runs due checks with low concurrency, at most one per site, with a
// pause between two checks of the same site (§5.4). A check never holds a
// download slot.
type Loop struct {
	d    Deps
	wake chan struct{}
	done chan doneMsg

	mu           sync.Mutex
	running      map[int64]bool
	full         map[int64]bool // full rescan requested
	nightlyTried map[int64]bool // by source id
	siteBusy     map[string]bool
	siteNext     map[string]time.Time
}

// NewLoop creates the check loop; Run starts it.
func NewLoop(d Deps) *Loop {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	return &Loop{d: d, wake: make(chan struct{}, 1), done: make(chan doneMsg, 16),
		running: map[int64]bool{}, full: map[int64]bool{}, nightlyTried: map[int64]bool{},
		siteBusy: map[string]bool{}, siteNext: map[string]time.Time{}}
}

// Wake makes the loop look for due subscriptions now.
func (l *Loop) Wake() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// RequestFull makes the next check of a subscription list every source
// completely (Full rescan).
func (l *Loop) RequestFull(id int64) {
	l.mu.Lock()
	l.full[id] = true
	l.mu.Unlock()
}

// Running reports whether a subscription is being checked.
func (l *Loop) Running(id int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running[id]
}

// Run checks due subscriptions until ctx is done, then waits for the
// running checks (they are interrupted and commit nothing).
func (l *Loop) Run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	var wg sync.WaitGroup
	for {
		wakeAt := l.dispatch(ctx, &wg)
		timer.Reset(max(time.Until(wakeAt), 10*time.Millisecond))
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-l.wake:
		case m := <-l.done:
			l.finished(m)
		case <-timer.C:
		}
	}
}

func (l *Loop) finished(m doneMsg) {
	s := l.d.Host.Settings().Subscriptions
	delay := time.Duration(float64(s.SiteDelaySeconds) * float64(time.Second) * (1 + 0.2*(rand.Float64()*2-1)))
	l.mu.Lock()
	delete(l.running, m.id)
	delete(l.siteBusy, m.site)
	l.siteNext[m.site] = l.d.Now().Add(delay)
	l.mu.Unlock()
	l.d.Host.Changed(m.id) // no longer checking
}

// dispatch starts due checks within the limits and returns when to look
// again.
func (l *Loop) dispatch(ctx context.Context, wg *sync.WaitGroup) time.Time {
	now := l.d.Now()
	wakeAt := now.Add(time.Hour)
	if ctx.Err() != nil {
		return wakeAt
	}
	settings := l.d.Host.Settings().Subscriptions
	if settings.Paused {
		return wakeAt
	}
	consider := func(t time.Time) {
		if t.After(now) && t.Before(wakeAt) {
			wakeAt = t
		}
	}
	if next, err := l.d.Store.NextCheckAt(ctx); err == nil {
		consider(next)
	}
	due, err := l.d.Store.DueSubscriptions(ctx, now)
	if err != nil {
		l.d.Logf("subscriptions: %v", err)
		return now.Add(time.Minute)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range l.siteNext {
		consider(t)
	}
	for _, sub := range due {
		if l.running[sub.ID] {
			continue
		}
		if len(l.running) >= settings.CheckConcurrency {
			break
		}
		opts, site, optErr := l.d.Host.Resolve(ctx, sub)
		if l.siteBusy[site] || now.Before(l.siteNext[site]) {
			continue
		}
		if cd := l.d.Host.Cooldown(site); cd.After(now) {
			// The site is rate-limited: postpone until the cooldown ends.
			if _, err := l.d.Store.SetNextCheck(ctx, []int64{sub.ID}, cd); err == nil {
				consider(cd)
			}
			continue
		}
		full := l.full[sub.ID]
		delete(l.full, sub.ID)
		l.running[sub.ID] = true
		l.siteBusy[site] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.check(ctx, sub, opts, site, optErr, full)
			l.done <- doneMsg{id: sub.ID, site: site}
		}()
	}
	return wakeAt
}

// Hints of a check that could not use its download options.
const (
	HintOptions       = "Edit the subscription's download options, or restore what it uses."
	HintCookieProfile = "Upload the cookie profile again or choose another one."
)

// IsOptionsError reports whether the last check of sub failed on its
// download options (a removed library, cookie profile or preset).
func IsOptionsError(sub *store.Subscription) bool {
	return sub.ErrorClass != "" && (sub.Hint == HintOptions || sub.Hint == HintCookieProfile)
}

// check runs one check: list every source, then commit.
func (l *Loop) check(ctx context.Context, sub *store.Subscription, opts config.Options, site string, optErr error, full bool) {
	bg := context.WithoutCancel(ctx)
	if err := l.d.Store.SetSubscriptionState(bg, sub.ID, store.SubChecking); err != nil {
		return
	}
	l.d.Host.Changed(sub.ID)
	log := l.d.Logs.Get(sub.ID)
	defer l.d.Logs.Release(sub.ID)
	now := l.d.Now()
	res := &Result{SubscriptionID: sub.ID, Started: now, Options: opts}
	log.Printf("=== Check %s ===", now.Format("2006-01-02 15:04:05"))
	if optErr != nil {
		log.Printf("--- Download options: %v. Nothing is listed or recorded ---", optErr)
		res.OptionsErr = &ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: optErr.Error(),
			Hint: HintOptions}
		l.commit(ctx, res, log)
		return
	}
	settings := l.d.Host.Settings()
	bin, release, err := l.d.Host.Acquire(ctx, opts.YtdlpVersion)
	if err != nil {
		if ctx.Err() != nil {
			l.abort(sub.ID)
			return
		}
		log.Printf("--- No usable yt-dlp: %v ---", err)
		for _, src := range sub.Sources {
			res.Sources = append(res.Sources, SourceResult{SourceID: src.ID,
				Err: &ytdlp.Classification{Class: ytdlp.ClassTransient, Reason: "no usable yt-dlp version: " + err.Error()}})
		}
		l.commit(ctx, res, log)
		return
	}
	defer release()
	res.Ytdlp = bin.ID
	log.Printf("yt-dlp %s (%s)", bin.Version, bin.ID)

	call := ytdlp.CallOptions{Impersonate: opts.Impersonate, SleepRequests: opts.SleepRequests, CacheDir: l.d.CacheDir}
	if opts.CookieProfile != "" {
		dir, err := os.MkdirTemp(l.d.TmpDir, "check-"+strconv.FormatInt(sub.ID, 10)+"-")
		if err == nil {
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, ytdlp.CookieFile)
			if err = l.d.Host.CopyCookies(opts.CookieProfile, path); err == nil {
				call.CookiesFile = path
			}
		}
		if err != nil {
			log.Printf("--- Cookie profile %q: %v ---", opts.CookieProfile, err)
			res.OptionsErr = &ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: "cookie profile: " + err.Error(),
				Hint: HintCookieProfile}
			l.commit(ctx, res, log)
			return
		}
	}
	known, err := l.d.Store.EntryKeys(bg, sub.ID)
	if err != nil {
		known = map[string]bool{}
	}
	for _, src := range sub.Sources {
		if res.Cooldown.After(now) {
			// Rate-limited: don't list the other sources of the site now.
			res.Sources = append(res.Sources, SourceResult{SourceID: src.ID,
				Err: &ytdlp.Classification{Class: ytdlp.ClassRateLimited, Reason: "rate-limited by the site"}})
			continue
		}
		r := l.listSource(ctx, sub, src, bin, call, settings, known, full, log)
		if ctx.Err() != nil {
			l.abort(sub.ID)
			return
		}
		if r.Err != nil && r.Err.Class == ytdlp.ClassRateLimited {
			res.Cooldown = l.d.Now().Add(time.Duration(settings.RateLimitCooldownMinutes) * time.Minute)
			l.d.Host.SetCooldown(site, res.Cooldown)
			log.Printf("--- Rate-limited: site cooldown until %s ---", res.Cooldown.Format("15:04"))
		}
		res.Sources = append(res.Sources, r)
	}
	_ = failpoint.Check("subscription:listed")
	l.commit(ctx, res, log)
}

func (l *Loop) commit(ctx context.Context, res *Result, log *joblog.Log) {
	if err := l.d.Host.Commit(context.WithoutCancel(ctx), res); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			l.d.Logs.Delete(res.SubscriptionID) // deleted during the check
			return
		}
		log.Printf("--- Recording the check failed: %v ---", err)
		l.d.Logf("subscriptions: commit of %d failed: %v", res.SubscriptionID, err)
		l.abort(res.SubscriptionID)
	}
}

// abort puts an interrupted check back to idle; it runs again soon.
func (l *Loop) abort(id int64) {
	bg := context.Background()
	_ = l.d.Store.SetSubscriptionState(bg, id, store.SubIdle)
	l.d.Host.Changed(id)
}

// listSource lists one source: a window for newest-first sources (doubled
// while it contains no known entry), a full listing otherwise (§5.2).
func (l *Loop) listSource(ctx context.Context, sub *store.Subscription, src *store.Source, bin versions.Binary,
	call ytdlp.CallOptions, settings config.Settings, known map[string]bool, full bool, log *joblog.Log) SourceResult {
	s := settings.Subscriptions
	now := l.d.Now()
	r := SourceResult{SourceID: src.ID}
	window := 0
	switch {
	case full, src.FirstOKAt.IsZero(), sub.Listing == store.ListingFull:
	case sub.Listing == store.ListingAuto && !Windowed(src):
	case src.LastFullAt.IsZero() || now.Sub(src.LastFullAt) > time.Duration(s.FullRescanDays)*24*time.Hour:
	default:
		window = s.Window
	}
	// A source that was blocked lists with impersonation; once that worked
	// it keeps it until the options change (§5.4).
	if call.Impersonate == "" && (src.Impersonate || src.ErrorClass == string(ytdlp.ClassBlocked)) && bin.ImpersonateAvailable {
		call.Impersonate = "any"
		r.Impersonated = true
	}
	// extractor_broken: the next check uses the latest nightly once.
	if src.ErrorClass == string(ytdlp.ClassExtractorBroken) && settings.Ytdlp.NightlyRetryOnBroken {
		l.mu.Lock()
		tried := l.nightlyTried[src.ID]
		l.nightlyTried[src.ID] = true
		l.mu.Unlock()
		if !tried && (bin.Channel != config.ChannelNightly || !l.d.Host.IsLatest(ctx, bin)) {
			if nb, rel, err := l.d.Host.Acquire(ctx, config.ChannelNightly+":latest"); err == nil {
				defer rel()
				bin = nb
				r.Nightly = true
				log.Printf("Retrying with yt-dlp %s (%s)", nb.Version, nb.ID)
			}
		}
	}
	for {
		r.Window = window
		timeout := FullTimeout
		desc := "full listing"
		if window > 0 {
			timeout, desc = WindowTimeout, fmt.Sprintf("newest %d", window)
		}
		if r.Impersonated {
			desc += ", impersonating"
		}
		log.Printf("Listing %s (%s)", src.Canonical, desc)
		lctx, cancel := context.WithTimeout(ctx, timeout)
		var out []string
		res := ytdlp.Run(lctx, ytdlp.Call{Bin: bin.Path, CaptureStdout: true,
			Args:   ytdlp.ListArgs(src.Canonical, ytdlp.ListSpec{Window: window, YouTubeTab: src.Tab != ""}, call),
			OnLine: func(line string) { out = append(out, line) }})
		timedOut := lctx.Err() != nil && ctx.Err() == nil
		cancel()
		if ctx.Err() != nil {
			return r
		}
		if !res.OK() {
			cls := ytdlp.Classify(res)
			if timedOut {
				cls = ytdlp.Classification{Class: ytdlp.ClassTransient, Reason: "listing timed out after " + timeout.String()}
			}
			for _, line := range tail(out, 20) {
				log.Line("  " + line)
			}
			log.Printf("--- Failed (%s: %s) ---", cls.Class, cls.Reason)
			r.Err, r.Output = &cls, out
			return r
		}
		listing, err := ytdlp.ParseListing(res.Stdout)
		if err != nil {
			log.Printf("--- Failed: %v ---", err)
			r.Err = &ytdlp.Classification{Class: ytdlp.ClassUnknown, Reason: err.Error()}
			return r
		}
		r.Listing = listing
		if window == 0 || len(listing.Entries) < window {
			r.Complete = true
			log.Printf("Listed %d entries", len(listing.Entries))
			return r
		}
		gap := true
		for _, e := range listing.Entries {
			if known[EntryKey(e)] {
				gap = false
				break
			}
		}
		if !gap {
			log.Printf("Listed %d entries", len(listing.Entries))
			return r
		}
		window *= 2
		if window > maxWindow {
			window = 0
		}
		log.Printf("No known entry among them: more than %d new videos, listing more", len(listing.Entries))
	}
}

func tail(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

// SummaryLine formats the result line of a check log.
func SummaryLine(s Summary, queued int, jobIDs []int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Checked %d source", s.Sources)
	if s.Sources != 1 {
		b.WriteString("s")
	}
	fmt.Fprintf(&b, ": %d listed, %d new", s.Listed, s.New)
	if s.Backlog > 0 {
		fmt.Fprintf(&b, ", %d backlog", s.Backlog)
	}
	if queued > 0 {
		var ids []string
		for _, id := range jobIDs {
			ids = append(ids, "#"+strconv.FormatInt(id, 10))
		}
		fmt.Fprintf(&b, ", %d queued (job %s)", queued, strings.Join(ids, ", "))
	}
	if s.Filtered > 0 {
		fmt.Fprintf(&b, ", %d ignored by filter", s.Filtered)
	}
	if s.Gone > 0 {
		fmt.Fprintf(&b, ", %d gone", s.Gone)
	}
	if s.Failed > 0 {
		fmt.Fprintf(&b, ", %d failed", s.Failed)
	}
	return b.String()
}
