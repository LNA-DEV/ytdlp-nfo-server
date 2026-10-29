package subscriptions

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/joblog"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// Loading older posts (Reddit concept §7.7): a backfill lists posts once
// and records them as backlog entries of the subscription's subreddit
// source. A check runs a pending request after its normal listing and
// commits its entries and the request's progress in the same transaction.

// BackfillRequest is one run of a backfill.
type BackfillRequest struct {
	URL       string // the subreddit or user listing (new posts)
	Providers string // the subscription's provider chain
	Backfill  store.Backfill
	// Filters become server-side filters of a range (media kinds as URL
	// prefixes, NSFW as over_18).
	Filters store.Filters
	Timeout time.Duration
}

// BackfillResult is the outcome of a run.
type BackfillResult struct {
	SourceID int64
	ID       int64          // the request that ran
	Listing  *ytdlp.Listing // nil on failure
	Err      *ytdlp.Classification
	// Next is the request after the run: nil when it is done, else its
	// position for Continue or the error of a run to repeat.
	Next     *store.Backfill
	Download bool // queue the new entries in the backlog band
	Output   []string
	Provider string
}

// Backfiller loads older posts.
type Backfiller interface {
	Backfill(ctx context.Context, req BackfillRequest) *BackfillResult
}

// ServerFilter is one search of a range: posts whose URL starts with
// Prefix ("" = any), NSFW or SFW posts only (Over18; nil = both).
type ServerFilter struct {
	Prefix string
	Over18 *bool
}

// mediaPrefixes are the URL prefixes of the media kinds (§7.7).
var mediaPrefixes = map[string]string{
	"image":   "https://i.redd.it/",
	"gif":     "https://i.redd.it/",
	"video":   "https://v.redd.it/",
	"gallery": "https://www.reddit.com/gallery/",
}

// ServerFilters turns the subscription's filters into the searches of a
// range: one per URL prefix of its media kinds, so text and link posts are
// never transferred. Links to other sites need a search without a prefix.
func ServerFilters(f store.Filters) []ServerFilter {
	var over18 *bool
	switch f.NSFW {
	case store.NSFWExclude:
		over18 = new(bool)
	case store.NSFWOnly:
		v := true
		over18 = &v
	}
	if len(f.MediaKinds) == 0 || slices.Contains(f.MediaKinds, MediaLinked) {
		return []ServerFilter{{Over18: over18}}
	}
	var out []ServerFilter
	seen := map[string]bool{}
	for _, k := range f.MediaKinds {
		if p := mediaPrefixes[k]; p != "" && !seen[p] {
			seen[p] = true
			out = append(out, ServerFilter{Prefix: p, Over18: over18})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Prefix < out[j].Prefix })
	return out
}

// Backfill implements Backfiller.
func (r *RedditLister) Backfill(ctx context.Context, req BackfillRequest) *BackfillResult {
	var log lines
	b := req.Backfill
	res := &BackfillResult{ID: b.ID, Download: b.Download}
	// A failed run is repeated by the next check; one that can't succeed
	// waits until the user changes or cancels it.
	fail := func(c ytdlp.Classification) *BackfillResult {
		res.Err, res.Output = &c, log.get()
		next := b
		next.Error = c.Reason
		if c.Class == ytdlp.ClassPermanent {
			next.Pending = false
		}
		res.Next = &next
		return res
	}
	t, ok := reddit.Parse(req.URL)
	if !ok || !t.IsListing() {
		return fail(ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: "not a Reddit subreddit or user"})
	}
	t.Sort, t.Period = "", ""
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	chain := r.Chain(req.Providers, log.logf)
	maxPosts := max(r.Settings().BackfillMax, 1)
	var posts []reddit.Post
	next := b
	next.Error = ""
	done := true
	switch b.Kind {
	case store.BackfillTop:
		top := t
		top.Sort, top.Period = reddit.SortTop, b.Period
		page, name, err := chain.List(ctx, reddit.Query{Target: top, Limit: min(max(b.N, 1), reddit.MaxListing)})
		if err != nil {
			c := err.Classification()
			c.Hint = reddit.HintTopNative + " " + c.Hint
			return fail(c)
		}
		posts, res.Provider = page.Posts, name
	case store.BackfillRange, store.BackfillRangeTop:
		before := b.To
		if !b.Before.IsZero() {
			before = b.Before
		}
		seen := map[string]bool{}
		for _, f := range ServerFilters(req.Filters) {
			// One more than the cap tells whether the range holds more.
			page, name, err := chain.List(ctx, reddit.Query{Target: t, After: b.From, Before: before, ByTime: true,
				Limit: maxPosts + 1, URLPrefix: f.Prefix, Over18: f.Over18})
			if err != nil {
				return fail(err.Classification())
			}
			res.Provider = name
			for _, p := range page.Posts {
				if !seen[p.ID] {
					seen[p.ID] = true
					posts = append(posts, p)
				}
			}
		}
		sort.SliceStable(posts, func(i, j int) bool { return posts[i].Created.After(posts[j].Created) })
		switch {
		case b.Kind == store.BackfillRangeTop && len(posts) > maxPosts:
			return fail(ytdlp.Classification{Class: ytdlp.ClassPermanent,
				Reason: fmt.Sprintf("more than %d posts in this range; a top list of a range sorts the whole range, so narrow it", maxPosts)})
		case b.Kind == store.BackfillRangeTop:
			sort.SliceStable(posts, func(i, j int) bool { return posts[i].Score > posts[j].Score })
			posts = posts[:min(len(posts), max(b.N, 1))]
		case len(posts) > maxPosts:
			// Stop at the cap; Continue resumes before the oldest post
			// loaded (the posts of its second come again and are known).
			posts = posts[:maxPosts]
			next.Before, next.Pending, done = posts[len(posts)-1].Created, false, false
		}
	default:
		return fail(ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: "unknown backfill " + b.Kind})
	}
	next.Loaded += len(posts)
	if !done {
		res.Next = &next
	}
	res.Listing = toListing(ctx, t, &reddit.Page{Posts: posts}, r.links())
	res.Output = log.get()
	return res
}

// BackfillDesc describes a request for the log and the UI.
func BackfillDesc(b *store.Backfill) string {
	day := func(t time.Time, open string) string {
		if t.IsZero() {
			return open
		}
		return t.UTC().Format("2006-01-02")
	}
	switch b.Kind {
	case store.BackfillTop:
		if b.Period == "all" {
			return fmt.Sprintf("top %d of all time", b.N)
		}
		return fmt.Sprintf("top %d of the %s", b.N, b.Period)
	case store.BackfillRangeTop:
		return fmt.Sprintf("top %d of %s – %s", b.N, day(b.From, "the beginning"), day(b.To, "now"))
	}
	return fmt.Sprintf("posts of %s – %s", day(b.From, "the beginning"), day(b.To, "now"))
}

// runBackfill runs a pending backfill of a subscription after its normal
// listing, on its first Reddit source.
func (l *Loop) runBackfill(ctx context.Context, sub *store.Subscription, log *joblog.Log) *BackfillResult {
	b := sub.Backfill
	for _, src := range sub.Sources {
		t, ok := reddit.Parse(src.Canonical)
		if !ok || !t.IsListing() {
			continue
		}
		bf, ok := l.d.ListerFor(src.Canonical).(Backfiller)
		if !ok {
			break
		}
		t.Sort, t.Period = "", ""
		if b.Loaded > 0 {
			log.Printf("Loading older posts: %s, continuing before %s", BackfillDesc(b), b.Before.UTC().Format("2006-01-02 15:04"))
		} else {
			log.Printf("Loading older posts: %s", BackfillDesc(b))
		}
		res := bf.Backfill(ctx, BackfillRequest{URL: t.Canonical(), Providers: sub.Provider, Backfill: *b, Filters: sub.Filters,
			Timeout: FullTimeout})
		res.SourceID = src.ID
		for _, line := range res.Output {
			log.Line("  " + line)
		}
		switch {
		case res.Err != nil:
			log.Printf("--- Loading older posts failed (%s: %s) ---", res.Err.Class, res.Err.Reason)
		case res.Next != nil:
			log.Printf("Listed %d older posts via %s; %d so far, stopped at the limit (Continue loads more)",
				len(res.Listing.Entries), res.Provider, res.Next.Loaded)
		default:
			log.Printf("Listed %d older posts via %s", len(res.Listing.Entries), res.Provider)
		}
		return res
	}
	log.Printf("--- Loading older posts: the subscription has no Reddit subreddit or user source ---")
	next := *b
	next.Pending, next.Error = false, "older posts can only be loaded for Reddit subreddits and users"
	return &BackfillResult{ID: b.ID, Next: &next, Err: &ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: next.Error}}
}
