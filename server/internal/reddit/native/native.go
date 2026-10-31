package native

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// MaxPage limits one HTML document (§5.1).
const MaxPage = 5 << 20

// Provider is the native provider.
type Provider struct {
	Client *browser.Client
	// PageDelay is the pause between two pages of one listing (2–5 s,
	// random, §5.1). Tests set it to zero.
	PageDelay func() time.Duration
	// MinGap is the least time between two page loads, also of different
	// requests (the posts of a job, §5.1). Tests set it to zero.
	MinGap time.Duration
	// Solve answers a JavaScript check page and returns the URL its form
	// submits to (package jschallenge); nil, or ErrNoAnswer, leaves such
	// pages blocked.
	Solve func(ctx context.Context, page []byte, pageURL *url.URL) (string, error)

	mu   sync.Mutex
	last time.Time // when the last page load started
	// spelling maps lower-cased subreddit names to their own spelling:
	// Reddit renders a feed only for that one (§5.3).
	spelling map[string]string
}

// ErrNoAnswer is returned by a Solve that doesn't answer checks (the
// setting is off).
var ErrNoAnswer = errors.New("checks are not answered")

// New creates the provider.
func New(c *browser.Client) *Provider {
	return &Provider{Client: c, MinGap: 2 * time.Second, PageDelay: func() time.Duration {
		return 2*time.Second + time.Duration(rand.Int64N(int64(3*time.Second)))
	}}
}

// pace waits until MinGap has passed since the last page load.
func (p *Provider) pace(ctx context.Context) error {
	p.mu.Lock()
	wait := max(time.Until(p.last.Add(p.MinGap)), 0)
	p.last = time.Now().Add(wait)
	p.mu.Unlock()
	return sleep(ctx, func() time.Duration { return wait })
}

// Name implements reddit.Provider.
func (p *Provider) Name() string { return reddit.ProviderNative }

// Site implements reddit.Provider: the key of native's own cooldowns. It
// isn't the site key of Reddit jobs and checks, so a blocked native
// doesn't hold them up while Arctic Shift can answer.
func (p *Provider) Site() string { return SiteKey }

// SiteKey is the key of native's cooldowns.
const SiteKey = "reddit-native"

// ListingURL returns the first page of a listing (§5.1).
func ListingURL(t reddit.Target) string {
	q := url.Values{"feedViewType": {"compactView"}}
	sort := t.EffectiveSort()
	var base string
	switch t.Kind {
	case reddit.TargetSubreddit:
		base = "https://www.reddit.com/r/" + t.Subreddit + "/" + sort + "/"
		if t.Period != "" {
			q.Set("t", t.Period)
		}
	case reddit.TargetUser:
		base = "https://www.reddit.com/user/" + t.User + "/submitted/"
		if sort != reddit.SortNew {
			q.Set("sort", sort)
		}
		if t.Period != "" {
			q.Set("t", t.Period)
		}
	default:
		return ""
	}
	return base + "?" + q.Encode()
}

// moreURL builds the next page of a subreddit listing from a cursor, as
// the page's script does (§5.1). User pages only follow their partial.
func moreURL(t reddit.Target, cursor string) string {
	if t.Kind != reddit.TargetSubreddit || cursor == "" {
		return ""
	}
	q := url.Values{"after": {cursor}, "name": {t.Subreddit}, "feedViewType": {"compactView"}}
	if t.Period != "" {
		q.Set("t", strings.ToUpper(t.Period))
	}
	return "https://www.reddit.com/svc/shreddit/community-more-posts/" + t.EffectiveSort() + "/?" + q.Encode()
}

// cursorFor returns the cursor of the post after which the next page
// starts: base64 of its full name, as observed (§5.2).
func cursorFor(id string) string { return base64.StdEncoding.EncodeToString([]byte("t3_" + id)) }

// spelled returns a target with the subreddit's own spelling, if known.
func (p *Provider) spelled(t reddit.Target) reddit.Target {
	if t.Kind != reddit.TargetSubreddit {
		return t
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.spelling[strings.ToLower(t.Subreddit)]; s != "" {
		t.Subreddit = s
	}
	return t
}

// learn remembers the spelling of a subreddit seen on a page.
func (p *Provider) learn(name string) {
	if name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spelling == nil {
		p.spelling = map[string]string{}
	}
	p.spelling[strings.ToLower(name)] = name
}

// List implements reddit.Provider.
func (p *Provider) List(ctx context.Context, q reddit.Query) (*reddit.Page, error) {
	if q.TimeRange() {
		return nil, &reddit.Error{Class: ytdlp.ClassPermanent, Reason: "native can't list a time range", Unsupported: true}
	}
	q.Target = p.spelled(q.Target)
	first := ListingURL(q.Target)
	if first == "" {
		return nil, &reddit.Error{Class: ytdlp.ClassPermanent, Reason: "not a listing", Unsupported: true}
	}
	limit := min(max(q.Limit, 1), reddit.MaxListing)
	out := &reddit.Page{Title: q.Target.Name()}
	seen := map[string]bool{}
	next, referer := first, ""
	for n := 0; next != ""; n++ {
		if n > 0 {
			if err := sleep(ctx, p.PageDelay); err != nil {
				return nil, err
			}
		}
		var resp *browser.Response
		var u *url.URL
		var pg page
		var err error
		if n == 0 {
			resp, u, pg, err = p.load(ctx, next, func(pg page) bool { return pg.feed || len(pg.posts) > 0 })
		} else if err = p.pace(ctx); err == nil {
			if resp, err = p.Client.Partial(ctx, next, referer, MaxPage); err == nil {
				u, pg = parsed(resp, next)
			}
		}
		if err != nil {
			return nil, transport(err)
		}
		if e := classify(resp, u, pg, n == 0, p.Client); e != nil {
			if n > 0 && e.Class != ytdlp.ClassRateLimited && e.Class != ytdlp.ClassBlocked {
				out.End = true // a later page failed: keep what we have
				break
			}
			return nil, e
		}
		if n == 0 {
			if pg.title != "" {
				out.Title = pg.title
			}
			out.Icon = pg.icon
			referer = first
			if q.Target.Kind == reddit.TargetSubreddit {
				if name := subredditOf(pg, u); strings.EqualFold(name, q.Target.Subreddit) && name != q.Target.Subreddit {
					p.learn(name)
					q.Target.Subreddit = name // the next pages ask for it by name
					referer = ListingURL(q.Target)
				}
			}
		}
		added := 0
		for _, ps := range pg.posts {
			if seen[ps.ID] {
				continue
			}
			seen[ps.ID] = true
			if ps.Subreddit == "" {
				ps.Subreddit = q.Target.Subreddit
			}
			if ps.Permalink == "" {
				ps.Permalink = reddit.PostURL(ps.Subreddit, ps.ID)
			}
			out.Posts = append(out.Posts, ps)
			added++
		}
		reddit.Logf(ctx, "native: page %d, %d new posts", n+1, added)
		if len(out.Posts) >= limit {
			out.Posts = out.Posts[:limit]
			break
		}
		switch {
		case added == 0 && n > 0:
			next = ""
		case pg.more != "" && pg.more != next:
			next = pg.more
		case pg.cursor != "":
			next = moreURL(q.Target, pg.cursor)
		case len(pg.posts) > 0:
			next = moreURL(q.Target, cursorFor(pg.posts[len(pg.posts)-1].ID))
		default:
			next = ""
		}
		if next == "" {
			out.End = true
		}
	}
	return out, nil
}

// Post implements reddit.Provider.
func (p *Provider) Post(ctx context.Context, id, subreddit string) (*reddit.Post, error) {
	if subreddit != "" {
		subreddit = p.spelled(reddit.Target{Kind: reddit.TargetSubreddit, Subreddit: subreddit}).Subreddit
	}
	resp, base, pg, err := p.load(ctx, reddit.PostURL(subreddit, id), func(pg page) bool {
		for _, ps := range pg.posts {
			if ps.ID == id {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, transport(err)
	}
	if e := classify(resp, base, pg, true, p.Client); e != nil {
		return nil, e
	}
	for i := range pg.posts {
		if pg.posts[i].ID == id {
			ps := pg.posts[i]
			return &ps, nil
		}
	}
	if resp.Status == 200 {
		return nil, &reddit.Error{Class: ytdlp.ClassPermanent, Reason: "post not found (removed or deleted)"}
	}
	return nil, &reddit.Error{Class: ytdlp.ClassExtractorBroken, Reason: "post page without the post", Hint: reddit.HintLayout,
		Snippet: snippet(resp.Body)}
}

// load loads and parses a page. A page without what the caller wants
// (done) whose script only redirects is followed once, as a browser
// does: Reddit answers a post URL without its title, and a subreddit in
// another spelling, with such a page (§5.1).
func (p *Provider) load(ctx context.Context, u string, done func(page) bool) (*browser.Response, *url.URL, page, error) {
	resp, err := p.document(ctx, u, "")
	if err != nil {
		return nil, nil, page{}, err
	}
	base, pg := parsed(resp, u)
	if done(pg) || pg.redirect == "" {
		return resp, base, pg, nil
	}
	target, err := url.Parse(pg.redirect)
	if err != nil || target.Scheme != "https" || !strings.EqualFold(target.Host, base.Host) || target.String() == base.String() {
		return resp, base, pg, nil
	}
	reddit.Logf(ctx, "native: %s redirects to %s", base.Path, target.Path)
	if resp, err = p.document(ctx, target.String(), base.String()); err != nil {
		return nil, nil, page{}, err
	}
	base, pg = parsed(resp, target.String())
	return resp, base, pg, nil
}

// parsed parses a response; its URL is the final one after HTTP redirects.
func parsed(resp *browser.Response, requested string) (*url.URL, page) {
	u, err := url.Parse(resp.URL)
	if err != nil || resp.URL == "" {
		u, _ = url.Parse(requested)
	}
	return u, parse(resp.Body, u)
}

// subredditOf returns the subreddit's spelling on a listing page: from
// its page data, else from its URL.
func subredditOf(pg page, u *url.URL) string {
	if pg.name != "" {
		return pg.name
	}
	if parts := strings.Split(strings.Trim(u.Path, "/"), "/"); len(parts) >= 2 && parts[0] == "r" {
		return parts[1]
	}
	return ""
}

// document loads a page. A JavaScript check instead of the page is
// answered once if Solve is set; the page that follows is classified as
// usual, and another check there counts as blocked.
func (p *Provider) document(ctx context.Context, u, referer string) (*browser.Response, error) {
	if err := p.pace(ctx); err != nil {
		return nil, err
	}
	resp, err := p.Client.Document(ctx, u, referer, MaxPage)
	if err != nil || p.Solve == nil || !isChallenge(resp.Body) {
		return resp, err
	}
	pageURL, err := url.Parse(resp.URL)
	if err != nil || resp.URL == "" {
		pageURL, _ = url.Parse(u)
	}
	submit, err := p.Solve(ctx, resp.Body, pageURL)
	if errors.Is(err, ErrNoAnswer) {
		return resp, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		p.Client.Jar().Reset()
		return nil, &reddit.Error{Class: ytdlp.ClassBlocked, Reason: "Reddit's JavaScript check could not be answered: " + err.Error(),
			Hint: reddit.HintChallengeFailed}
	}
	reddit.Logf(ctx, "native: answered Reddit's JavaScript check")
	resp, err = p.Client.Document(ctx, submit, pageURL.String(), MaxPage)
	if err == nil && isChallenge(resp.Body) {
		p.Client.Jar().Reset()
		return nil, &reddit.Error{Class: ytdlp.ClassBlocked, Reason: "Reddit did not accept the answer to its JavaScript check",
			Hint: reddit.HintChallengeFailed, Snippet: snippet(resp.Body)}
	}
	return resp, err
}

func isChallenge(body []byte) bool { return marker(strings.ToLower(string(body)), Markers.Challenge) }

// classify turns a response without usable posts into an error (§5.4).
// first: the document is a page (not a partial), where a missing feed is a
// layout error.
func classify(resp *browser.Response, u *url.URL, pg page, first bool, c *browser.Client) *reddit.Error {
	lower := strings.ToLower(string(resp.Body))
	switch {
	case resp.Status == 429 || marker(lower, Markers.RateLimit) && len(pg.posts) == 0:
		return &reddit.Error{Class: ytdlp.ClassRateLimited, Reason: "rate-limited by Reddit", RetryAfter: retryAfter(resp)}
	case marker(lower, Markers.Challenge) && len(pg.posts) == 0:
		c.Jar().Reset()
		return &reddit.Error{Class: ytdlp.ClassBlocked, Reason: "Reddit asks for a JavaScript check", Hint: reddit.HintChallenge,
			Snippet: snippet(resp.Body)}
	case marker(lower, Markers.Blocked) && len(pg.posts) == 0:
		c.Jar().Reset()
		return &reddit.Error{Class: ytdlp.ClassBlocked, Reason: "blocked by Reddit's network security", Hint: reddit.HintBlocked,
			Snippet: snippet(resp.Body)}
	case len(pg.posts) > 0:
		return nil
	case u != nil && (strings.Contains(u.Path, "/login") || strings.Contains(u.Path, "/account/")):
		return &reddit.Error{Class: ytdlp.ClassAuthRequired, Reason: "Reddit asks to log in", Hint: reddit.HintAuth}
	case marker(lower, Markers.Banned) || marker(lower, Markers.Private) || resp.Status == 404 || marker(lower, Markers.NotFound):
		return &reddit.Error{Class: ytdlp.ClassPermanent, Reason: "the subreddit or user does not exist, was banned or is private",
			Hint: reddit.HintNotFound}
	case pg.quarantined || marker(lower, Markers.Quarantine) || marker(lower, Markers.AgeGate):
		return &reddit.Error{Class: ytdlp.ClassAuthRequired, Reason: "Reddit withholds this content from logged-out visitors",
			Hint: reddit.HintAuth}
	case resp.Status == 403:
		c.Jar().Reset()
		return &reddit.Error{Class: ytdlp.ClassBlocked, Reason: "HTTP 403 from Reddit", Hint: reddit.HintBlocked, Snippet: snippet(resp.Body)}
	case resp.Status >= 500:
		return &reddit.Error{Class: ytdlp.ClassTransient, Reason: fmt.Sprintf("HTTP %d from Reddit", resp.Status)}
	case resp.Status != 200:
		return &reddit.Error{Class: ytdlp.ClassUnknown, Reason: fmt.Sprintf("HTTP %d from Reddit", resp.Status)}
	case !first, pg.feed, marker(lower, Markers.Empty):
		return nil // an empty listing, or the end of it
	}
	return &reddit.Error{Class: ytdlp.ClassExtractorBroken, Reason: "no posts found in Reddit's page", Hint: reddit.HintLayout,
		Snippet: snippet(resp.Body)}
}

func retryAfter(resp *browser.Response) time.Duration {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 0
}

// transport classifies an error of a request: a network error, or the
// error of an unanswered check.
func transport(err error) *reddit.Error {
	var e *reddit.Error
	if errors.As(err, &e) {
		return e
	}
	return &reddit.Error{Class: ytdlp.ClassTransient, Reason: err.Error()}
}

func snippet(b []byte) string {
	if len(b) > 2048 {
		b = b[:2048]
	}
	return string(b)
}

func sleep(ctx context.Context, d func() time.Duration) error {
	if d == nil {
		return nil
	}
	t := time.NewTimer(d())
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
