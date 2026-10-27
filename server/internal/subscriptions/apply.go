package subscriptions

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// SourceResult is the outcome of listing one source.
type SourceResult struct {
	SourceID int64
	Listing  *ytdlp.Listing // nil on failure
	// Complete: the listing holds every entry of the source (a full
	// listing, or a window that reached the end).
	Complete     bool
	Window       int // 0 = full listing
	Err          *ytdlp.Classification
	Impersonated bool // listed with escalated impersonation
	Nightly      bool // listed with the latest nightly
	Output       []string
}

// Result is the outcome of the listing phase of a check.
type Result struct {
	SubscriptionID int64
	Started        time.Time
	Options        config.Options
	// OptionsErr: the download options could not be resolved, so the check
	// lists nothing and records nothing (§5.2 "options first").
	OptionsErr *ytdlp.Classification
	Sources    []SourceResult
	Cooldown   time.Time // rate-limited: no check before this
	Ytdlp      string    // version id used
}

// Input is everything Apply needs; Sub and Entries are read in the commit
// transaction, so edits and deletes during the check are respected (§5.5).
type Input struct {
	Now        time.Time
	Sub        *store.Subscription
	Entries    []*store.Entry
	Archived   map[string]bool // archive membership of listed keys without an entry
	Result     *Result
	Settings   config.SubscriptionSettings
	HasCookies bool // the resolved options have a cookie profile
	Jitter     func() float64
}

// Summary counts what a check did (for the check log).
type Summary struct {
	Sources  int
	Failed   int
	Listed   int
	New      int // discovered after the first listing of their source
	Backlog  int // found by a first listing
	Filtered int
	Gone     int
}

// Output is the plan of a commit. Entries in Auto, Requeue and Backlog are
// also in Insert or Update; the manager links them to the items it creates.
type Output struct {
	Insert  []*store.Entry // in chronological order (oldest first)
	Update  []*store.Entry
	Auto    []*store.Entry // one auto job
	Requeue []*store.Entry // one one-item auto job each (§5.3)
	Backlog []*store.Entry // one backlog job (the backlog choice)
	Sources []*store.Source
	Summary Summary
	// EnteredError: the subscription went into the error state with this
	// check; CapHit: it hit autoQueueCap (notifications are sent once).
	EnteredError bool
	CapHit       bool
	// NewVisible counts the discovered entries that are not ignored (the
	// "3 new videos" of a notification).
	NewVisible int
}

// EntryKey returns the key of a listing entry: its archive key, or
// "url <normalized url>" if the listing has no extractor key or id.
func EntryKey(le ytdlp.ListEntry) string {
	if k := le.Key(); k != "" {
		return k
	}
	if config.ValidateURL(le.URL) == nil {
		return "url " + config.NormalizeURL(le.URL)
	}
	return ""
}

// listingDate returns the upload date (YYYYMMDD) and the sort time of a
// listing entry (zero if unknown).
func listingDate(le ytdlp.ListEntry) (string, time.Time) {
	if le.Timestamp > 0 {
		t := time.Unix(le.Timestamp, 0)
		date := le.UploadDate
		if date == "" {
			date = t.UTC().Format("20060102")
		}
		return date, t
	}
	if le.UploadDate != "" {
		if t, err := time.Parse("20060102", le.UploadDate); err == nil {
			return le.UploadDate, t.Add(12 * time.Hour)
		}
	}
	return "", time.Time{}
}

// IsCapWarning reports whether a subscription warning is the cap warning.
func IsCapWarning(w string) bool {
	return strings.HasPrefix(w, "Found ") && strings.Contains(w, " new videos (limit ")
}

// CapWarning is the warning of a check that hit autoQueueCap (S8).
func CapWarning(n, limit int) string {
	return fmt.Sprintf("Found %d new videos (limit %d). Review them before downloading.", n, limit)
}

func needsAuth(availability string) bool {
	switch availability {
	case "subscriber_only", "premium_only", "needs_auth":
		return true
	}
	return false
}

// classRank orders error classes for "the subscription shows the worst
// error".
func classRank(c ytdlp.Class) int {
	switch c {
	case ytdlp.ClassPermanent:
		return 8
	case ytdlp.ClassAuthRequired:
		return 7
	case ytdlp.ClassBlocked:
		return 6
	case ytdlp.ClassExtractorBroken:
		return 5
	case ytdlp.ClassRateLimited:
		return 4
	case ytdlp.ClassCrash:
		return 3
	case ytdlp.ClassUnknown:
		return 2
	}
	return 1
}

// Hint returns the hint shown with a check error.
func Hint(c ytdlp.Classification) string {
	switch c.Class {
	case ytdlp.ClassPermanent:
		if c.Hint != "" && c.Hint != ytdlp.HintPermanent {
			return c.Hint
		}
		return "The channel or playlist may have been removed or made private. Checks continue once a day."
	case ytdlp.ClassAuthRequired:
		return "Needs cookies: choose a cookie profile for this subscription or its site."
	case ytdlp.ClassBlocked:
		return ytdlp.HintBlocked
	case ytdlp.ClassExtractorBroken:
		return ytdlp.HintExtractorBroken
	case ytdlp.ClassRateLimited:
		return ytdlp.HintRateLimited
	case ytdlp.ClassCrash:
		return ytdlp.HintCrash
	}
	if c.Hint != "" {
		return c.Hint
	}
	return "The next check retries."
}

// Interval returns the effective check interval of a subscription.
func Interval(sub *store.Subscription, s config.SubscriptionSettings) time.Duration {
	m := sub.IntervalMinutes
	if m <= 0 {
		m = s.IntervalMinutes
	}
	return time.Duration(max(m, config.MinIntervalMinutes)) * time.Minute
}

func jitter(in *Input) float64 {
	if in.Jitter != nil {
		return in.Jitter()
	}
	return rand.Float64()*2 - 1
}

// nextCheck schedules the next check: the interval with ±10 % jitter after
// a success, min(interval × 2^(failures−1), 24 h) after a failed check.
func nextCheck(in *Input, failures int) time.Time {
	iv := Interval(in.Sub, in.Settings)
	if failures == 0 {
		return in.Now.Add(time.Duration(float64(iv) * (1 + 0.1*jitter(in))))
	}
	d := iv
	for i := 1; i < failures && d < 24*time.Hour; i++ {
		d *= 2
	}
	return in.Now.Add(min(d, 24*time.Hour))
}

// ApplyOptionsError records a check that could not resolve its download
// options: nothing is listed or recorded, so new videos are not used up.
func ApplyOptionsError(in Input) *Output {
	sub, cls := in.Sub, in.Result.OptionsErr
	out := &Output{EnteredError: sub.ErrorClass == ""}
	sub.LastCheckAt = in.Now
	sub.State = store.SubError
	sub.Failures++
	sub.Error, sub.ErrorClass, sub.Hint = cls.Reason, string(cls.Class), Hint(*cls)
	sub.NextCheckAt = in.Now.Add(24 * time.Hour)
	return out
}

// Apply diffs the listings of a check against the stored entries and plans
// the commit (§5.2, §5.3). It changes Sub and the entries in place.
func Apply(in Input) *Output {
	if in.Result.OptionsErr != nil {
		return ApplyOptionsError(in)
	}
	now, sub := in.Now, in.Sub
	out := &Output{}
	filter, err := CompileFilters(sub.Filters)
	if err != nil {
		filter = nil // validated on save; never block a check on it
	}
	byKey := map[string]*store.Entry{}
	for _, e := range in.Entries {
		byKey[e.Key] = e
	}
	srcByID := map[int64]*store.Source{}
	for _, src := range sub.Sources {
		srcByID[src.ID] = src
	}
	changed := map[*store.Entry]bool{}
	inserted := map[*store.Entry]bool{}
	seen := map[string]bool{}
	var discovered, becameAvailable []*store.Entry
	var worst *ytdlp.Classification
	listed, ok, complete := 0, 0, 0
	wasError := sub.ErrorClass != ""

	for i := range in.Result.Sources {
		r := &in.Result.Sources[i]
		src := srcByID[r.SourceID]
		if src == nil {
			continue // removed during the check: its results are dropped
		}
		listed++
		out.Sources = append(out.Sources, src)
		out.Summary.Sources++
		if r.Err != nil {
			out.Summary.Failed++
			src.Error, src.ErrorClass = r.Err.Reason, string(r.Err.Class)
			if worst == nil || classRank(r.Err.Class) > classRank(worst.Class) {
				worst = r.Err
			}
			continue
		}
		ok++
		if r.Complete {
			complete++
		}
		first := src.FirstOKAt.IsZero()
		entries := r.Listing.Entries
		order := make([]int, len(entries))
		for j := range order {
			order[j] = j
		}
		if src.Tab != "" { // newest first: insert oldest first
			for a, b := 0, len(order)-1; a < b; a, b = a+1, b-1 {
				order[a], order[b] = order[b], order[a]
			}
		}
		out.Summary.Listed += len(entries)
		for _, pos := range order {
			le := entries[pos]
			key := EntryKey(le)
			if key == "" {
				continue
			}
			date, sortTime := listingDate(le)
			e := byKey[key]
			if e == nil {
				e = &store.Entry{SubscriptionID: sub.ID, Key: key, URL: le.URL, SourceID: src.ID, Position: pos,
					Backlog: first, FirstSeenAt: now, LastSeenAt: now, SortDate: now}
				if sortTime.IsZero() {
					// Unknown dates keep the listing order (positions are
					// newest first in a tab, oldest first in a playlist).
					e.SortDate = now.Add(-time.Duration(pos) * time.Millisecond)
					if src.Tab == "" {
						e.SortDate = now.Add(-time.Duration(len(entries)-pos) * time.Millisecond)
					}
				}
				if le.Nested {
					e.Ignored, e.IgnoreReason = true, ReasonNotAVideo
				}
				byKey[key] = e
				inserted[e] = true
				out.Insert = append(out.Insert, e)
				if first {
					out.Summary.Backlog++
				} else {
					out.Summary.New++
					discovered = append(discovered, e)
				}
			} else if !inserted[e] {
				if e.SourceID == src.ID || e.SourceID == 0 || srcByID[e.SourceID] == nil {
					e.SourceID, e.Position = src.ID, pos
					if config.ValidateURL(le.URL) == nil {
						e.URL = le.URL
					}
				}
				if IsWaitingLiveStatus(e.LiveStatus) && !IsWaitingLiveStatus(le.LiveStatus) && !seen[key] {
					becameAvailable = append(becameAvailable, e)
				}
				e.LiveStatus = le.LiveStatus
				e.GoneAt, e.LastSeenAt = time.Time{}, now
				changed[e] = true
			}
			if seen[key] && !inserted[e] {
				continue
			}
			seen[key] = true
			// Listing fields come from the newest listing of any source.
			if le.Title != "" {
				e.Title = le.Title
			}
			if le.Duration > 0 {
				e.Duration = le.Duration
			}
			if date != "" {
				e.UploadDate, e.SortDate = date, sortTime
			}
			if le.Thumbnail != "" {
				e.Thumbnail = le.Thumbnail
			}
			e.LiveStatus, e.Availability = le.LiveStatus, le.Availability
		}
		if first {
			src.FirstOKAt = now
		}
		src.LastOKAt = now
		if r.Complete {
			src.LastFullAt = now
		}
		if r.Impersonated {
			src.Impersonate = true
		}
		src.Error, src.ErrorClass = "", ""
		if src == sub.Sources[0] && r.Listing != nil {
			l := r.Listing
			sub.Kind = Kind(l)
			if u := firstNonEmpty(l.Channel, l.Uploader); u != "" {
				sub.Uploader = u
			}
			if l.Thumbnail != "" {
				sub.Thumbnail = l.Thumbnail
			}
		}
	}

	// Gone: only after complete listings of all sources succeeded (§5.2).
	if listed == len(sub.Sources) && ok == listed && complete == listed && listed > 0 {
		for _, e := range in.Entries {
			if !seen[e.Key] && e.GoneAt.IsZero() {
				e.GoneAt = now
				changed[e] = true
				out.Summary.Gone++
			}
		}
	}

	// Filters apply to new entries in both modes (§4.1).
	for _, e := range out.Insert {
		if !e.Ignored {
			if r := filter.Reason(e); r != "" {
				e.Ignored, e.IgnoreReason = true, r
				out.Summary.Filtered++
			}
		}
	}

	for _, e := range discovered {
		if !e.Ignored {
			out.NewVisible++
		}
	}
	archived := func(e *store.Entry) bool { return e.InArchive || in.Archived[e.Key] }
	auto := sub.Mode == store.ModeAuto && sub.Enabled

	// Members-only without cookies (§5.3): auto mode ignores them instead
	// of queueing a download that fails; they come back with cookies.
	for _, e := range append(append([]*store.Entry(nil), in.Entries...), out.Insert...) {
		switch {
		case e.Ignored && e.IgnoreReason == ReasonNeedsCookie && in.HasCookies:
			e.Ignored, e.IgnoreReason = false, ""
			changed[e] = true
		case auto && !in.HasCookies && !e.Ignored && seen[e.Key] && needsAuth(e.Availability) && !archived(e) && !e.ItemActive():
			e.Ignored, e.IgnoreReason = true, ReasonNeedsCookie
			changed[e] = true
		}
	}

	if auto {
		var cands []*store.Entry
		for _, e := range discovered {
			if !e.Ignored && !archived(e) && !IsWaitingLiveStatus(e.LiveStatus) {
				cands = append(cands, e)
			}
		}
		capN := max(in.Settings.AutoQueueCap, 1)
		if len(cands) > capN {
			out.CapHit = !IsCapWarning(sub.Warning)
			sub.Warning = CapWarning(len(cands), capN)
		} else {
			if IsCapWarning(sub.Warning) {
				sub.Warning = ""
			}
			out.Auto = cands
		}
		// Upcoming entries that became available, and the classifier path
		// ("not available yet" without live_status), each get a new
		// one-item job: the only queueing after discovery (S3).
		queued := map[*store.Entry]bool{}
		since := now.AddDate(0, 0, -max(in.Settings.UpcomingDays, 1))
		requeue := func(e *store.Entry) {
			if queued[e] || e.Ignored || e.Backlog || archived(e) || e.ItemActive() || IsWaitingLiveStatus(e.LiveStatus) {
				return
			}
			if e.ItemID != 0 && e.ItemState != store.ItemSkipped && !e.NotYetAvailable() {
				return // it failed for another reason: S3
			}
			queued[e] = true
			out.Requeue = append(out.Requeue, e)
		}
		for _, e := range becameAvailable {
			requeue(e)
		}
		for _, e := range in.Entries {
			if e.NotYetAvailable() && e.FirstSeenAt.After(since) {
				requeue(e)
			}
		}
	}

	// The backlog choice is applied once the first listings of all
	// sources succeeded, then cleared (§5.2).
	if sub.Backlog.Pending() && sub.Enabled && allFirstOK(sub) {
		var cands []*store.Entry
		for _, e := range append(append([]*store.Entry(nil), in.Entries...), out.Insert...) {
			if e.Backlog && !e.Ignored && !archived(e) && e.ItemID == 0 && !IsWaitingLiveStatus(e.LiveStatus) {
				cands = append(cands, e)
			}
		}
		if sub.Backlog.Mode == store.BacklogLatest && len(cands) > sub.Backlog.N {
			SortNewestFirst(cands, srcByID)
			cands = cands[:sub.Backlog.N]
		}
		out.Backlog = cands
		sub.Backlog = store.BacklogChoice{}
	}

	for _, e := range in.Entries {
		if changed[e] {
			out.Update = append(out.Update, e)
		}
	}

	// Check state.
	sub.LastCheckAt = now
	if ok > 0 {
		sub.LastOKAt = now
	}
	if worst == nil {
		sub.State, sub.Error, sub.ErrorClass, sub.Hint, sub.Failures = store.SubIdle, "", "", "", 0
		sub.NextCheckAt = nextCheck(&in, 0)
	} else {
		sub.State = store.SubError
		sub.Error, sub.ErrorClass, sub.Hint = worst.Reason, string(worst.Class), Hint(*worst)
		if ok > 0 {
			sub.Failures = 0
			sub.NextCheckAt = nextCheck(&in, 0)
		} else {
			sub.Failures++
			sub.NextCheckAt = nextCheck(&in, sub.Failures)
			if worst.Class == ytdlp.ClassPermanent {
				sub.NextCheckAt = now.Add(24 * time.Hour)
			}
		}
		out.EnteredError = !wasError
	}
	if in.Result.Cooldown.After(sub.NextCheckAt) {
		sub.NextCheckAt = in.Result.Cooldown
	}
	return out
}

func allFirstOK(sub *store.Subscription) bool {
	for _, src := range sub.Sources {
		if src.FirstOKAt.IsZero() {
			return false
		}
	}
	return len(sub.Sources) > 0
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// newerInSource compares two entries of the same source by position:
// lower positions are newer in a channel tab, higher ones in a playlist.
func newerInSource(a, b *store.Entry, srcByID map[int64]*store.Source) bool {
	if src := srcByID[a.SourceID]; src != nil && src.Tab != "" {
		return a.Position < b.Position
	}
	return a.Position > b.Position
}

// SortNewestFirst orders entries by sort_date, newest first; equal dates
// by position within one source. Positions of different sources are never
// compared.
func SortNewestFirst(es []*store.Entry, srcByID map[int64]*store.Source) {
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if !a.SortDate.Equal(b.SortDate) {
			return a.SortDate.After(b.SortDate)
		}
		if a.SourceID == b.SourceID && a.SourceID != 0 {
			return newerInSource(a, b, srcByID)
		}
		return false
	})
}

// SortOldestFirst orders the items of a job: oldest first, so videos reach
// the library in the order they were published (§4.3).
func SortOldestFirst(es []*store.Entry, srcByID map[int64]*store.Source) {
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if !a.SortDate.Equal(b.SortDate) {
			return a.SortDate.Before(b.SortDate)
		}
		if a.SourceID == b.SourceID && a.SourceID != 0 {
			return newerInSource(b, a, srcByID)
		}
		return false
	})
}

// IsWaitingLiveStatus reports whether the video is upcoming, live or just
// ended (still processing).
func IsWaitingLiveStatus(s string) bool { return store.IsWaitingLiveStatus(s) }
