// Package native reads the HTML pages a logged-out browser gets from
// www.reddit.com (Reddit concept §5). It never runs JavaScript.
package native

// Layout holds every name the parser relies on (§5.2). They are what third
// parties observed on Reddit's pages; `reddit-probe` shows the real pages,
// and this table is the one place to correct them.
var Layout = struct {
	PostTag     string // one element per post
	FeedTag     string // the feed of a listing page
	PartialTag  string // a lazily loaded part of the page
	MorePosts   string // part of the src of the partial that loads more posts
	GalleryTag  string // the image carousel of a gallery post
	PlayerTags  []string
	HeaderTag   string   // the community header (title, icon)
	ID          []string // post ID ("t3_<id>")
	Permalink   string
	Title       string
	Author      string
	Subreddit   []string // "r/<name>" or "<name>"
	Created     string
	Type        string
	ContentHref string
	Score       string
	NSFW        []string
	Spoiler     []string
	Cursor      string
	HeaderTitle []string
	HeaderIcon  []string

	// PageData holds the community as JSON (name, icon, NSFW and
	// quarantine flags) in its data attribute.
	PageData string
	// RedirectID is the element whose script call (RedirectCall, method
	// "location.replace(...)") sends a browser to the canonical URL: Reddit
	// answers a post URL without its title, or a subreddit in another
	// spelling, with an otherwise empty page holding only this redirect.
	RedirectID   string
	RedirectCall string
}{
	PostTag:     "shreddit-post",
	FeedTag:     "shreddit-feed",
	PartialTag:  "faceplate-partial",
	MorePosts:   "more-posts",
	GalleryTag:  "gallery-carousel",
	PlayerTags:  []string{"shreddit-player", "shreddit-player-2"},
	HeaderTag:   "shreddit-subreddit-header",
	ID:          []string{"id", "post-id", "thingid"},
	Permalink:   "permalink",
	Title:       "post-title",
	Author:      "author",
	Subreddit:   []string{"subreddit-prefixed-name", "subreddit-name"},
	Created:     "created-timestamp",
	Type:        "post-type",
	ContentHref: "content-href",
	Score:       "score",
	NSFW:        []string{"nsfw", "is-nsfw", "over18"},
	Spoiler:     []string{"spoiler", "is-spoiler"},
	Cursor:      "more-posts-cursor",
	HeaderTitle: []string{"display-name", "prefixed-name"},
	HeaderIcon:  []string{"icon"},

	PageData:     "reddit-page-data",
	RedirectID:   "shreddit-redirect",
	RedirectCall: "ac-call",
}

// Markers are texts that identify special pages when a page has no posts
// (§5.4). They are matched case-insensitively.
var Markers = struct {
	Challenge  []string // a JavaScript check instead of the page
	Blocked    []string
	RateLimit  []string
	Banned     []string
	NotFound   []string
	Private    []string
	Quarantine []string
	AgeGate    []string
	Empty      []string
}{
	Challenge:  []string{`name="js_challenge"`, `name="jsc_token"`},
	Blocked:    []string{"blocked by network security", "you've been blocked"},
	RateLimit:  []string{"you've been rate limited", "too many requests"},
	Banned:     []string{"has been banned", "community is banned"},
	NotFound:   []string{"community not found", "nobody on reddit goes by that name", "page not found"},
	Private:    []string{"this community is private", "private community"},
	Quarantine: quarantined,
	AgeGate:    []string{"verify your age", "age verification", "you must be 18"},
	Empty:      []string{"there doesn't seem to be anything here", "no posts yet", "hasn't posted yet"},
}

// quarantined marks a quarantine gate. Every community page has
// "quarantined":false in its data, so only a true value or the gate's text
// counts.
var quarantined = []string{"this community has been quarantined", `&quot;isquarantined&quot;:true`, `"isquarantined":true`,
	`&quot;quarantined&quot;:true`, `"quarantined":true`}
