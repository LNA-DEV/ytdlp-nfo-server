package native

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
)

// page is what the parser found in one HTML document.
type page struct {
	posts  []reddit.Post
	cursor string // the last more-posts cursor
	more   string // absolute URL of the partial that loads more posts
	feed   bool   // a feed element was present
	title  string
	icon   string
	// From the page data: the community's own spelling, and whether
	// Reddit quarantined it.
	name        string
	quarantined bool
	// redirect is the absolute URL the page's script redirects to.
	redirect string
}

// redirectCall reads the target of a script call like
// location.replace("/r/Example/comments/abc/title/").
var redirectCall = regexp.MustCompile(`^\s*(?:window\.)?location\.(?:replace|assign)\(\s*(["'])([^"']+)["']\s*\)\s*;?\s*$`)

var jsUnescape = strings.NewReplacer(`\/`, "/", `\u002F`, "/", `\u002f`, "/", `\u0026`, "&")

// pageData is the part of the page data the parser reads.
type pageData struct {
	Subreddit *struct {
		Name          string `json:"name"`
		Icon          string `json:"communityIcon"`
		IsQuarantined bool   `json:"isQuarantined"`
	} `json:"subreddit"`
}

type postState struct {
	post     reddit.Post
	hint     string
	content  string
	gallery  []reddit.Media
	seenImg  map[string]int // media id → index in gallery
	videoURL string
	inner    *reddit.Post // a nested post: the original of a crosspost
}

func attrMap(t html.Token) map[string]string {
	m := make(map[string]string, len(t.Attr))
	for _, a := range t.Attr {
		m[strings.ToLower(a.Key)] = a.Val
	}
	return m
}

func first(m map[string]string, keys []string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	return ""
}

// truthy reads a boolean attribute: present without value, or "true".
func truthy(m map[string]string, keys []string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok && (v == "" || strings.EqualFold(v, "true") || v == "1") {
			return true
		}
	}
	return false
}

var timeLayouts = []string{
	time.RFC3339Nano, "2006-01-02T15:04:05.999999-0700", "2006-01-02T15:04:05-0700", "2006-01-02T15:04:05.999999Z0700",
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 1e9 {
		if n > 1e12 {
			n /= 1000
		}
		return time.Unix(int64(n), 0).UTC()
	}
	return time.Time{}
}

func absolute(base *url.URL, ref string) string {
	if ref == "" {
		return ""
	}
	u, err := base.Parse(ref)
	if err != nil {
		return ""
	}
	return u.String()
}

// imageCandidates returns the image URLs of an img or source element.
func imageCandidates(m map[string]string) []string {
	var out []string
	for _, k := range []string{"src", "data-lazy-src", "data-src"} {
		if v := m[k]; v != "" {
			out = append(out, v)
		}
	}
	if ss := m["srcset"]; ss != "" {
		for _, part := range strings.Split(ss, ",") {
			if f := strings.Fields(part); len(f) > 0 {
				out = append(out, f[0])
			}
		}
	}
	return out
}

// parse reads one document: a listing page, a partial with more posts, or
// a post page.
func parse(body []byte, base *url.URL) page {
	var pg page
	z := html.NewTokenizer(bytes.NewReader(body))
	// Open post elements (crossposts can nest) and gallery elements are
	// closed by their end tags; other elements may be left open in HTML.
	var stack []*postState
	inGallery := 0
	// Div depth inside the redirect element, 0 outside of it.
	inRedirect := 0
	build := func(ps *postState) reddit.Post {
		p := ps.post
		content := ps.content
		if ps.videoURL != "" && reddit.VideoIDFromURL(content) == "" {
			if id := reddit.VideoIDFromURL(ps.videoURL); id != "" && (ps.hint == "video" || content == "") {
				content = ps.videoURL
			}
		}
		p.Classify(content, ps.hint)
		if p.Kind == reddit.KindGallery && len(ps.gallery) > 0 {
			p.Media = ps.gallery
		}
		if in := ps.inner; in != nil && in.ID != "" && in.ID != p.ID && (p.Kind == "" || p.CrosspostParent != "" || ps.hint == "crosspost") {
			// A crosspost shows the media of its original (§7.5).
			p.Kind, p.Media, p.VideoID, p.Link, p.Duration = in.Kind, in.Media, in.VideoID, in.Link, in.Duration
			p.CrosspostParent = in.ID
			if p.Thumbnail == "" {
				p.Thumbnail = in.Thumbnail
			}
		}
		return p
	}
	// finish closes the innermost open post.
	finish := func() {
		ps := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		p := build(ps)
		switch {
		case len(stack) > 0:
			stack[len(stack)-1].inner = &p
		case p.ID != "":
			pg.posts = append(pg.posts, p)
		}
	}
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			for len(stack) > 0 {
				finish()
			}
			return pg
		case html.StartTagToken, html.SelfClosingTagToken:
			t := z.Token()
			name := strings.ToLower(t.Data)
			self := tt == html.SelfClosingTagToken
			m := attrMap(t)
			var cur *postState
			if len(stack) > 0 {
				cur = stack[len(stack)-1]
			}
			switch {
			case name == Layout.PostTag:
				ps := &postState{hint: m[Layout.Type], content: absolute(base, m[Layout.ContentHref]),
					seenImg: map[string]int{}}
				id := strings.ToLower(first(m, Layout.ID))
				ps.post.ID = strings.TrimPrefix(id, "t3_")
				ps.post.Permalink = absolute(base, m[Layout.Permalink])
				ps.post.Title = m[Layout.Title]
				ps.post.Removed = removedTitle(ps.post.Title)
				ps.post.Author = m[Layout.Author]
				sub := first(m, Layout.Subreddit)
				ps.post.Subreddit = strings.TrimPrefix(strings.TrimPrefix(sub, "r/"), "u/")
				ps.post.Created = parseTime(m[Layout.Created])
				ps.post.NSFW = truthy(m, Layout.NSFW)
				ps.post.Spoiler = truthy(m, Layout.Spoiler)
				if n, err := strconv.Atoi(m[Layout.Score]); err == nil {
					ps.post.Score = n
				}
				ps.post.Provider = reddit.ProviderNative
				if c := m[Layout.Cursor]; c != "" {
					pg.cursor = c
				}
				stack = append(stack, ps)
				if self {
					finish()
				}
			case name == Layout.FeedTag:
				pg.feed = true
			case name == Layout.HeaderTag:
				if v := first(m, Layout.HeaderTitle); v != "" {
					pg.title = v
				}
				if v := first(m, Layout.HeaderIcon); v != "" {
					pg.icon = absolute(base, v)
				}
			case name == Layout.PageData:
				var d pageData
				if json.Unmarshal([]byte(m["data"]), &d) == nil && d.Subreddit != nil {
					pg.name, pg.quarantined = d.Subreddit.Name, d.Subreddit.IsQuarantined
					if pg.icon == "" && d.Subreddit.Icon != "" {
						pg.icon = absolute(base, d.Subreddit.Icon)
					}
				}
			case name == "div" && inRedirect > 0:
				if !self {
					inRedirect++
				}
			case name == "div" && m["id"] == Layout.RedirectID:
				if !self {
					inRedirect = 1
				}
			case name == Layout.RedirectCall && inRedirect > 0 && pg.redirect == "":
				if c := redirectCall.FindStringSubmatch(m["method"]); c != nil {
					pg.redirect = absolute(base, jsUnescape.Replace(c[2]))
				}
			case name == Layout.PartialTag:
				if src := m["src"]; strings.Contains(src, Layout.MorePosts) {
					pg.more = absolute(base, src)
				}
			case m["slot"] == Layout.RemovedSlot && cur != nil:
				cur.post.Removed = true
			case name == Layout.GalleryTag:
				if !self {
					inGallery++
				}
			case isPlayer(name) && cur != nil:
				for _, v := range m {
					if id := reddit.VideoIDFromURL(v); id != "" && cur.videoURL == "" {
						cur.videoURL = "https://v.redd.it/" + id
					}
				}
				if cur.videoURL == "" {
					if i := strings.Index(m["packaged-media-json"], "v.redd.it/"); i >= 0 {
						cur.videoURL = "https://" + m["packaged-media-json"][i:]
					}
				}
			case (name == "img" || name == "source") && cur != nil:
				for _, src := range imageCandidates(m) {
					src = absolute(base, src)
					if inGallery > 0 {
						// Each image is there several times: as previews
						// (whose names may start with the title) and as the
						// original on i.redd.it, whose extension wins.
						id, ext := reddit.ImageFromURL(src)
						if id == "" {
							continue
						}
						if i, ok := cur.seenImg[id]; ok {
							if strings.HasPrefix(src, "https://i.redd.it/") {
								cur.gallery[i].URL = reddit.OriginalImageURL(id, ext)
							}
							continue
						}
						cur.seenImg[id] = len(cur.gallery)
						cur.gallery = append(cur.gallery, reddit.Media{URL: reddit.OriginalImageURL(id, ext), MediaID: id,
							Caption: m["alt"]})
						continue
					}
					if cur.post.Thumbnail == "" && isThumbnailHost(src) {
						cur.post.Thumbnail = src
					}
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			n := strings.ToLower(string(name))
			if n == Layout.PostTag && len(stack) > 0 {
				finish()
			}
			if n == Layout.GalleryTag && inGallery > 0 {
				inGallery--
			}
			if n == "div" && inRedirect > 0 {
				inRedirect--
			}
		}
	}
}

// removedTitle reports whether a title is the one Reddit shows for a
// removed or deleted post.
func removedTitle(title string) bool {
	t := strings.ToLower(strings.Join(strings.Fields(title), ""))
	for _, r := range Layout.RemovedTitles {
		if t == r {
			return true
		}
	}
	return false
}

func isPlayer(name string) bool {
	for _, p := range Layout.PlayerTags {
		if name == p {
			return true
		}
	}
	return false
}

func isThumbnailHost(src string) bool {
	u, err := url.Parse(src)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "preview.redd.it", "external-preview.redd.it", "i.redd.it", "b.thumbs.redditmedia.com", "a.thumbs.redditmedia.com":
		return true
	}
	return false
}

// marker reports whether the lower-cased body contains one of the texts.
func marker(lower string, texts []string) bool {
	for _, t := range texts {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}
