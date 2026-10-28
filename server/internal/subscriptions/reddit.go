package subscriptions

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// RedditLister lists Reddit sources through the provider chain (Reddit
// concept §7.1).
type RedditLister struct {
	// Chain returns the provider chain for a subscription's override ("" =
	// the global chain); logf receives the chain's log lines.
	Chain    func(providers string, logf func(string, ...any)) *reddit.Chain
	Settings func() config.RedditSettings
	// Links resolves link posts (§7.6); nil follows none.
	Links func() reddit.Links
}

func (r *RedditLister) links() reddit.Links {
	if r.Links == nil {
		return reddit.Links{}
	}
	return r.Links()
}

// GapFiller lists the posts of a time range, to fill a gap (§7.7).
type GapFiller interface {
	FillGap(ctx context.Context, req ListRequest, after, before time.Time) (*ListResult, *ytdlp.Classification)
}

type lines struct {
	mu  sync.Mutex
	out []string
}

func (l *lines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out = append(l.out, fmt.Sprintf(format, args...))
}

func (l *lines) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.out...)
}

// List implements Lister. Reddit listings are never complete (R8); a
// listing that stopped at its limit is truncated, one that reached the end
// of the source is exhausted.
func (r *RedditLister) List(ctx context.Context, req ListRequest) (*ListResult, *ytdlp.Classification) {
	var log lines
	res := &ListResult{}
	t, ok := reddit.Parse(req.URL)
	if !ok {
		return res, &ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: "not a Reddit listing"}
	}
	chain := r.Chain(req.Providers, log.logf)
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	if t.Kind == reddit.TargetPost {
		// A post: the preview switches to its subreddit (§7.1).
		p, _, err := chain.Post(ctx, t.PostID, t.Subreddit)
		res.Output = log.get()
		if err != nil {
			c := err.Classification()
			return res, &c
		}
		l := &ytdlp.Listing{ID: p.ID, Title: p.Title, Entries: []ytdlp.ListEntry{PostEntry(ctx, p, r.links())}}
		if p.Subreddit != "" {
			l.ChannelURL, l.Channel = "https://www.reddit.com/r/"+p.Subreddit+"/", "r/"+p.Subreddit
		}
		res.Listing = l
		return res, nil
	}
	if !t.IsListing() {
		return res, &ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: "share links can't be listed; open the link and use the subreddit's URL"}
	}
	limit := req.Window
	if limit <= 0 {
		limit = r.Settings().FirstListing
	}
	limit = min(limit, reddit.MaxListing)
	page, name, err := chain.List(ctx, reddit.Query{Target: t, Limit: limit})
	res.Output = log.get()
	res.TimedOut = ctx.Err() == context.DeadlineExceeded
	if err != nil {
		c := err.Classification()
		if res.TimedOut {
			c = ytdlp.Classification{Class: ytdlp.ClassTransient, Reason: "listing timed out after " + req.Timeout.String()}
		}
		return res, &c
	}
	res.Listing = toListing(ctx, t, page, r.links())
	res.Provider = name
	res.Truncated = !page.End && len(page.Posts) >= limit && limit >= reddit.MaxListing
	res.Exhausted = page.End || limit >= reddit.MaxListing
	return res, nil
}

// FillGap implements GapFiller: posts of the time range, through the
// providers of the chain that can list by time (Arctic Shift).
func (r *RedditLister) FillGap(ctx context.Context, req ListRequest, after, before time.Time) (*ListResult, *ytdlp.Classification) {
	var log lines
	res := &ListResult{}
	s := r.Settings()
	t, ok := reddit.Parse(req.URL)
	if !ok || !t.IsListing() || !s.GapFill {
		return res, &ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: "gap fill is off"}
	}
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	page, name, err := r.Chain(req.Providers, log.logf).List(ctx, reddit.Query{Target: t, Limit: s.BackfillMax, After: after, Before: before})
	res.Output = log.get()
	if err != nil {
		c := err.Classification()
		return res, &c
	}
	res.Listing, res.Provider = toListing(ctx, t, page, r.links()), name
	res.Truncated = !page.End && len(page.Posts) >= s.BackfillMax
	res.Exhausted = true
	return res, nil
}

// toListing maps a page to a flat listing.
func toListing(ctx context.Context, t reddit.Target, page *reddit.Page, links reddit.Links) *ytdlp.Listing {
	l := &ytdlp.Listing{ID: t.Name(), Title: page.Title, Channel: t.Name(), ExtractorKey: "Reddit", WebpageURL: t.Canonical(),
		Thumbnail: page.Icon, IsPlaylist: true}
	// The page knows the name's case; the canonical URL doesn't.
	switch {
	case l.Title == "":
		l.Title = t.Name()
	case t.Subreddit != "" && strings.EqualFold(strings.TrimPrefix(l.Title, "r/"), t.Subreddit):
		l.Title = "r/" + strings.TrimPrefix(l.Title, "r/")
	case t.User != "" && strings.EqualFold(strings.TrimPrefix(l.Title, "u/"), t.User):
		l.Title = "u/" + strings.TrimPrefix(l.Title, "u/")
	}
	l.Channel = l.Title
	l.ChannelID = l.ID // a subreddit or user counts as a channel
	for i := range page.Posts {
		l.Entries = append(l.Entries, PostEntry(ctx, &page.Posts[i], links))
	}
	return l
}

// PostEntry maps a post to a listing entry: keyed by its media key if the
// listing shows the media, else by the post key (R2, §7.5). A link post
// to a followed site becomes an entry of its target (§7.6): its key is the
// target's key, and its URL the target's, except for an Imgur still image,
// which the Reddit pipeline downloads from the post.
func PostEntry(ctx context.Context, p *reddit.Post, links reddit.Links) ytdlp.ListEntry {
	ext, id := p.ListingKey()
	le := ytdlp.ListEntry{URL: p.Permalink, ExtractorKey: ext, ID: id, Title: p.Title, Duration: p.Duration,
		Thumbnail: p.Thumbnail, MediaKind: p.Kind, NSFW: p.NSFW, Link: p.Link}
	if le.URL == "" {
		le.URL = reddit.PostURL(p.Subreddit, p.ID)
	}
	if p.Kind == reddit.KindLink {
		if t, ok := links.Resolve(ctx, p.Link); ok {
			le.MediaKind, le.Link = MediaLinked, ""
			le.ExtractorKey, le.ID, _ = strings.Cut(t.Key, " ")
			if !t.Image {
				le.URL = t.URL
			}
			if strings.HasPrefix(t.Key, "url ") { // no known form: keyed by URL
				le.ExtractorKey, le.ID = "", ""
			}
		}
	}
	if le.ExtractorKey != reddit.ExtractorPost {
		le.AltKey = reddit.PostKey(p.ID)
	}
	if le.Title == "" {
		le.Title = p.ID
	}
	if !p.Created.IsZero() {
		le.Timestamp = p.Created.Unix()
	}
	return le
}

// FolderFromName returns the default folder of a Reddit subscription name:
// the name without the slash, which the sanitizer would turn into "⧸".
func FolderFromName(name string) string {
	if n, ok := strings.CutPrefix(name, "r/"); ok {
		return n
	}
	if n, ok := strings.CutPrefix(name, "u/"); ok {
		return "u_" + n
	}
	return name
}

// IsGapWarning reports whether a subscription warning is a gap warning.
func IsGapWarning(w string) bool {
	return strings.HasPrefix(w, "More than ") && strings.Contains(w, " since the last check")
}

// GapWarning is the warning of a check with a gap (§7.7).
func GapWarning(listed int, filled bool, left int, suggest time.Duration) string {
	w := fmt.Sprintf("More than %d new posts since the last check", listed)
	switch {
	case filled:
		w += "; the gap was filled from Arctic Shift"
	case left > 0:
		w += fmt.Sprintf("; about %d posts could not be listed", left)
	default:
		w += "; posts in between may be missing"
	}
	if suggest > 0 {
		w += ". Check at least every " + humanInterval(suggest)
	}
	return w + "."
}

func humanInterval(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d minutes", max(int(d.Minutes()), 15))
	}
}

// suggestInterval returns the interval at which about 500 posts appear,
// from the posting rate of a listing (§7.7).
func suggestInterval(l *ytdlp.Listing) time.Duration {
	var newest, oldest int64
	n := 0
	for _, e := range l.Entries {
		if e.Timestamp == 0 {
			continue
		}
		n++
		if newest == 0 || e.Timestamp > newest {
			newest = e.Timestamp
		}
		if oldest == 0 || e.Timestamp < oldest {
			oldest = e.Timestamp
		}
	}
	if n < 2 || newest <= oldest {
		return 0
	}
	per := time.Duration(newest-oldest) * time.Second / time.Duration(n)
	return (per * 500).Round(15 * time.Minute)
}

// gapOf returns the first source result of a check with a gap.
func gapOf(r *Result) *SourceResult {
	if r == nil {
		return nil
	}
	for i := range r.Sources {
		if r.Sources[i].Gap {
			return &r.Sources[i]
		}
	}
	return nil
}
