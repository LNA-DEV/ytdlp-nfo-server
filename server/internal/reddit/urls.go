// Package reddit lists subreddits and user profiles and fetches posts,
// through two providers: native (the HTML pages a logged-out browser gets)
// and Arctic Shift (a third-party archive). See docs/reddit-concept.md.
package reddit

import (
	"net/url"
	"regexp"
	"strings"
)

// TargetKind is what a Reddit URL points at.
type TargetKind int

// Target kinds.
const (
	TargetNone      TargetKind = iota
	TargetSubreddit            // a subreddit listing
	TargetUser                 // a user's posts
	TargetPost                 // one post
	TargetShare                // a share link (/r/<sub>/s/<code>) that has to be resolved
)

// Sort orders of a listing.
const (
	SortNew           = "new"
	SortHot           = "hot"
	SortTop           = "top"
	SortRising        = "rising"
	SortControversial = "controversial"
)

// Periods of top and controversial listings.
var periods = map[string]bool{"hour": true, "day": true, "week": true, "month": true, "year": true, "all": true}

// ValidPeriod reports whether p is a top-list period.
func ValidPeriod(p string) bool { return periods[p] }

// Target is a parsed Reddit URL.
type Target struct {
	Kind      TargetKind
	Subreddit string // without "r/", as written
	User      string // without "u/"
	PostID    string // base-36, lowercase
	Sort      string // listings: "" (the default) or one of the Sort constants
	Period    string // top and controversial: hour … all
	Share     string // share link path, e.g. /r/pics/s/AbC123
}

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{1,20}$`)
	userRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{2,30}$`)
	postIDRe = regexp.MustCompile(`^[a-z0-9]{2,12}$`)
)

// IsRedditHost reports whether host serves Reddit pages (not media).
func IsRedditHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	switch host {
	case "reddit.com", "www.reddit.com", "old.reddit.com", "new.reddit.com", "np.reddit.com", "m.reddit.com",
		"sh.reddit.com", "redd.it":
		return true
	}
	return false
}

// Parse parses a Reddit page URL. ok is false for anything else, including
// media URLs (i.redd.it, v.redd.it).
func Parse(raw string) (t Target, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !IsRedditHost(u.Hostname()) {
		return Target{}, false
	}
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	if strings.EqualFold(u.Hostname(), "redd.it") {
		if len(parts) == 1 && postIDRe.MatchString(strings.ToLower(parts[0])) {
			return Target{Kind: TargetPost, PostID: strings.ToLower(parts[0])}, true
		}
		return Target{}, false
	}
	q := u.Query()
	listing := func(t Target, rest []string) (Target, bool) {
		if len(rest) > 0 {
			switch s := strings.ToLower(rest[0]); s {
			case SortNew, SortHot, SortTop, SortRising, SortControversial:
				t.Sort = s
				rest = rest[1:]
			}
		}
		if len(rest) > 0 {
			return Target{}, false
		}
		if t.Sort == SortTop || t.Sort == SortControversial {
			t.Period = strings.ToLower(q.Get("t"))
			if !periods[t.Period] {
				t.Period = "day" // Reddit's default
			}
		}
		return t, true
	}
	post := func(t Target, id string) (Target, bool) {
		id = strings.ToLower(id)
		if !postIDRe.MatchString(id) {
			return Target{}, false
		}
		t.Kind, t.PostID = TargetPost, id
		return t, true
	}
	switch {
	case len(parts) >= 2 && parts[0] == "comments":
		return post(Target{}, parts[1])
	case len(parts) >= 2 && parts[0] == "gallery":
		return post(Target{}, parts[1])
	case len(parts) >= 2 && (parts[0] == "r" || parts[0] == "R"):
		sub := parts[1]
		if !nameRe.MatchString(sub) {
			return Target{}, false
		}
		t := Target{Kind: TargetSubreddit, Subreddit: sub}
		if len(parts) >= 4 && parts[2] == "comments" {
			return post(t, parts[3])
		}
		if len(parts) == 4 && parts[2] == "s" {
			return Target{Kind: TargetShare, Subreddit: sub, Share: "/r/" + sub + "/s/" + parts[3]}, true
		}
		return listing(t, parts[2:])
	case len(parts) >= 2 && (parts[0] == "user" || parts[0] == "u"):
		name := parts[1]
		if !userRe.MatchString(name) {
			return Target{}, false
		}
		t := Target{Kind: TargetUser, User: name}
		rest := parts[2:]
		if len(rest) >= 2 && rest[0] == "comments" {
			return post(t, rest[1])
		}
		if len(rest) == 2 && rest[0] == "s" {
			return Target{Kind: TargetShare, User: name, Share: "/user/" + name + "/s/" + rest[1]}, true
		}
		if len(rest) > 0 && (rest[0] == "submitted" || rest[0] == "posts") {
			rest = rest[1:]
		}
		if s := strings.ToLower(q.Get("sort")); s != "" && len(rest) == 0 {
			rest = []string{s}
		}
		return listing(t, rest)
	}
	return Target{}, false
}

// IsListing reports whether t is a subreddit or user listing.
func (t Target) IsListing() bool { return t.Kind == TargetSubreddit || t.Kind == TargetUser }

// EffectiveSort returns the sort order a listing uses: new unless the URL
// asked for another one.
func (t Target) EffectiveSort() string {
	if t.Sort == "" {
		return SortNew
	}
	return t.Sort
}

// Canonical returns the canonical URL of a listing (Reddit concept §7.1):
// https://www.reddit.com/r/<name lowercase>/new, …/top?t=<period>, or
// https://www.reddit.com/user/<name>/submitted. A post's canonical URL is
// https://www.reddit.com/comments/<id>/.
func (t Target) Canonical() string {
	switch t.Kind {
	case TargetSubreddit:
		base := "https://www.reddit.com/r/" + strings.ToLower(t.Subreddit)
		switch s := t.EffectiveSort(); s {
		case SortTop, SortControversial:
			return base + "/" + s + "?t=" + t.Period
		default:
			return base + "/" + s
		}
	case TargetUser:
		base := "https://www.reddit.com/user/" + t.User + "/submitted"
		switch s := t.EffectiveSort(); s {
		case SortNew:
			return base
		case SortTop, SortControversial:
			return base + "?sort=" + s + "&t=" + t.Period
		default:
			return base + "?sort=" + s
		}
	case TargetPost:
		return PostURL(t.Subreddit, t.PostID)
	case TargetShare:
		return "https://www.reddit.com" + t.Share
	}
	return ""
}

// Name returns the display name of a listing: r/<name> or u/<name>.
func (t Target) Name() string {
	switch {
	case t.Subreddit != "":
		return "r/" + t.Subreddit
	case t.User != "":
		return "u/" + t.User
	}
	return ""
}

// Folder returns the default library folder of a listing (§7.1): the name
// without the slash, which the sanitizer would turn into "⧸".
func (t Target) Folder() string {
	switch {
	case t.Subreddit != "":
		return t.Subreddit
	case t.User != "":
		return "u_" + t.User
	}
	return ""
}

// PostURL returns the permalink of a post.
func PostURL(sub, id string) string {
	if sub == "" {
		return "https://www.reddit.com/comments/" + id + "/"
	}
	return "https://www.reddit.com/r/" + sub + "/comments/" + id + "/"
}

// IsPostURL reports whether raw is the URL of one Reddit post.
func IsPostURL(raw string) bool {
	t, ok := Parse(raw)
	return ok && t.Kind == TargetPost
}

// RSSListing maps a Reddit RSS feed URL (…/.rss) to its listing URL, so an
// OPML import of a feed reader subscribes to the same listing (§11).
func RSSListing(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !IsRedditHost(u.Hostname()) {
		return "", false
	}
	p := strings.TrimSuffix(u.Path, "/")
	switch {
	case strings.HasSuffix(p, "/.rss"):
		p = strings.TrimSuffix(p, "/.rss")
	case strings.HasSuffix(p, ".rss"):
		p = strings.TrimSuffix(p, ".rss")
	default:
		return "", false
	}
	u.Path, u.RawPath = p, ""
	t, ok := Parse(u.String())
	if !ok || !t.IsListing() {
		return "", false
	}
	return t.Canonical(), true
}
