package fakereddit

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Post is a post of the fake site.
type Post struct {
	ID, Subreddit, Author, Title string
	Kind                         string // image, gif, gallery, video, text, link
	Created                      time.Time
	NSFW                         bool
	Media                        []string // image ids (gallery: several), or the v.redd.it id
	Link                         string
	Original                     *Post // a crosspost's original
	Removed                      bool  // the media is gone from i.redd.it
	// Ext is the extension the images were uploaded with (default png, gif
	// for a GIF); i.redd.it serves them only under it.
	Ext string
	// NoPreview leaves the preview URLs out of a gallery's Arctic Shift
	// metadata, so only the MIME type hints at the extension.
	NoPreview bool
	// Slug is the title part of the permalink (default "post").
	Slug string
	// Score ranks the post in top, hot, rising and controversial lists.
	Score int
}

// Site serves the same posts through native pages, the Arctic Shift API
// and the media hosts.
type Site struct {
	*Server
	mu            sync.Mutex
	subs          map[string][]*Post // lower-cased subreddit → posts, newest first
	byID          map[string]*Post
	nativeBlocked bool
	arcticDown    bool
	PageSize      int
	images        map[string][]byte
	gone          map[string]bool
	challenge     bool
	seeds         map[string]string // token → seed of a check page
	// Solved counts the JavaScript checks answered correctly.
	Solved int
	// names holds each subreddit's own spelling (of its first post), the
	// only one Reddit renders a feed for.
	names       map[string]string
	quarantined map[string]bool
	// arcticMaintenance counts the Arctic Shift requests still to answer
	// with 503.
	arcticMaintenance int
}

// NewSite starts a fake site.
func NewSite(t testing.TB) *Site {
	s := &Site{Server: New(t), subs: map[string][]*Post{}, byID: map[string]*Post{}, PageSize: 25, images: map[string][]byte{}, gone: map[string]bool{},
		seeds: map[string]string{}, names: map[string]string{}, quarantined: map[string]bool{}}
	s.Handle("www.reddit.com", s.native)
	s.Handle("reddit.com", s.native)
	s.Handle("arctic-shift.photon-reddit.com", s.arctic)
	s.Handle("i.redd.it", s.media)
	s.Handle("i.imgur.com", s.media)
	s.Handle("v.redd.it", s.video)
	return s
}

// Add adds posts (newest first after sorting by time).
func (s *Site) Add(posts ...*Post) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range posts {
		sub := strings.ToLower(p.Subreddit)
		if s.names[sub] == "" {
			s.names[sub] = p.Subreddit
		}
		s.subs[sub] = append(s.subs[sub], p)
		s.byID[p.ID] = p
		if p.Original != nil {
			s.byID[p.Original.ID] = p.Original
		}
		sort.SliceStable(s.subs[sub], func(i, j int) bool { return s.subs[sub][i].Created.After(s.subs[sub][j].Created) })
	}
}

// SetQuarantined makes a subreddit's page a quarantine gate without posts.
func (s *Site) SetQuarantined(sub string) {
	s.mu.Lock()
	s.quarantined[strings.ToLower(sub)] = true
	s.mu.Unlock()
}

// spelling returns a subreddit's own spelling.
func (s *Site) spelling(sub string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.names[strings.ToLower(sub)]; n != "" {
		return n
	}
	return sub
}

// ShellPage is what Reddit sends a first-time visitor of a post URL
// without its title, or of a subreddit in another spelling: no content,
// only a script call that redirects to the canonical URL, and data that
// mentions "quarantined" on every page.
func ShellPage(target string) []byte {
	t := html.EscapeString(target)
	return []byte(`<html><head><title>Reddit</title></head><body><shreddit-app pageType="post_detail"></shreddit-app>` +
		`<div id="canonical-url-updater" value="https://www.reddit.com` + t + `"></div>` +
		`<div id="shreddit-redirect"><ac-call delay="0" method="location.replace(&quot;` + t + `&quot;)" target="window" trigger="init"></ac-call></div>` +
		`<shreddit-async-loader data="{&quot;nsfw&quot;:false,&quot;quarantined&quot;:false}"></shreddit-async-loader></body></html>`)
}

// SetNativeBlocked makes native pages answer with the block page.
func (s *Site) SetNativeBlocked(b bool) { s.mu.Lock(); s.nativeBlocked = b; s.mu.Unlock() }

// ArcticMaintenance makes the next n Arctic Shift requests answer 503
// "Under maintenance".
func (s *Site) ArcticMaintenance(n int) { s.mu.Lock(); s.arcticMaintenance = n; s.mu.Unlock() }

// SetArcticDown makes the Arctic Shift API answer 500.
func (s *Site) SetArcticDown(b bool) { s.mu.Lock(); s.arcticDown = b; s.mu.Unlock() }

// SetChallenge makes native pages answer a client without the check's
// cookie with a JavaScript check shaped like Reddit's: an inline script
// puts seed+seed into a hidden GET form and submits it with the page's
// query. A correct answer sets the cookie and redirects to the page.
func (s *Site) SetChallenge(on bool) { s.mu.Lock(); s.challenge = on; s.mu.Unlock() }

// ChallengePage returns a check page for a seed, a token and the form's
// action.
func ChallengePage(seed, token, action string) []byte {
	script := `document.addEventListener("DOMContentLoaded",async function(){var e=document.forms[0],n=(e.onsubmit=function(t){` +
		`return new URLSearchParams(document.location.search).forEach((e,n)=>t.target.appendChild(Object.assign(` +
		`document.createElement("input"),{name:n,type:"hidden",value:e}))),!0},await(async e=>e+e)("` + seed + `"));` +
		`e.elements.namedItem("solution").value=n,e.requestSubmit()},{once:!0});`
	return []byte(`<!DOCTYPE html><html lang="en"><head><title>Reddit</title><script>` + script + `</script></head><body>` +
		`<form hidden method="GET" action="` + html.EscapeString(action) + `"><input type="hidden" name="solution" />` +
		`<input type="hidden" name="js_challenge" value="1"/><input type="hidden" name="jsc_token" value="` + token + `"/>` +
		`<input type="hidden" name="jsc_orig_r" value=""/></form></body></html>`)
}

// checkChallenge answers a request without the cookie: true if it was
// handled (a check page, or a redirect after a correct answer).
func (s *Site) checkChallenge(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.challenge {
		return false
	}
	if c, err := r.Cookie("jsc_ok"); err == nil && c.Value == "1" {
		return false
	}
	q := r.URL.Query()
	if q.Get("js_challenge") == "1" {
		if seed, ok := s.seeds[q.Get("jsc_token")]; ok && q.Get("solution") == seed+seed {
			delete(s.seeds, q.Get("jsc_token"))
			s.Solved++
			for _, k := range []string{"js_challenge", "solution", "jsc_token", "jsc_orig_r"} {
				q.Del(k)
			}
			http.SetCookie(w, &http.Cookie{Name: "jsc_ok", Value: "1", Path: "/", Domain: "reddit.com"})
			u := *r.URL
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.RequestURI(), http.StatusFound)
			return true
		}
	}
	n := len(s.seeds) + 1
	seed, token := fmt.Sprintf("seed%04d", n), fmt.Sprintf("token%04d", n)
	s.seeds[token] = seed
	w.Write(ChallengePage(seed, token, r.URL.Path))
	return true
}

// SetGone makes i.redd.it answer 404 for a media id.
func (s *Site) SetGone(id string) { s.mu.Lock(); s.gone[id] = true; s.mu.Unlock() }

// SetImage sets the bytes of an image id.
func (s *Site) SetImage(id string, b []byte) { s.mu.Lock(); s.images[id] = b; s.mu.Unlock() }

// PNG returns a small valid PNG whose pixels depend on seed.
func PNG(seed int) []byte {
	var b bytes.Buffer
	png.Encode(&b, picture(seed, ""))
	return b.Bytes()
}

// picture is an 80×60 gradient; the bytes of tag are written into its
// first row, so every media id gets its own image.
func picture(seed int, tag string) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 80, 60))
	for y := 0; y < 60; y++ {
		for x := 0; x < 80; x++ {
			img.Set(x, y, color.RGBA{uint8(x*3 + seed*7), uint8(y*4 + seed*13), uint8((x + y + seed) * 2), 255})
		}
	}
	for i := 0; i < len(tag) && i < 80; i++ {
		img.Set(i, 0, color.RGBA{tag[i], tag[i], tag[i], 255})
	}
	return img
}

// MediaBytes returns what the fake i.redd.it serves for a media id: a PNG,
// or a GIF for a .gif name, unless SetImage set other bytes. Every id gets
// its own picture (random color blocks), so different ids are neither
// identical nor similar.
func MediaBytes(id string, gifImage bool) []byte {
	if gifImage {
		return MediaBytesExt(id, "gif")
	}
	return MediaBytesExt(id, "png")
}

// MediaBytesExt returns the picture of a media id encoded for an
// extension: GIF, JPEG or PNG.
func MediaBytesExt(id, ext string) []byte {
	var b bytes.Buffer
	img := mediaPicture(id)
	if ext == "jpg" || ext == "jpeg" {
		jpeg.Encode(&b, img, &jpeg.Options{Quality: 90})
		return b.Bytes()
	}
	if ext == "gif" {
		pal := image.NewPaletted(img.Bounds(), palette.Plan9)
		draw.Draw(pal, pal.Bounds(), img, image.Point{}, draw.Src)
		gif.EncodeAll(&b, &gif.GIF{Image: []*image.Paletted{pal, pal}, Delay: []int{10, 10}})
		return b.Bytes()
	}
	png.Encode(&b, img)
	return b.Bytes()
}

func mediaPicture(id string) *image.RGBA {
	h := fnv.New64a()
	h.Write([]byte(id))
	r := rand.New(rand.NewPCG(h.Sum64(), 7))
	var grid [9][12]color.RGBA
	for y := range grid {
		for x := range grid[y] {
			grid[y][x] = color.RGBA{uint8(r.IntN(256)), uint8(r.IntN(256)), uint8(r.IntN(256)), 255}
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, 120, 90))
	for y := 0; y < 90; y++ {
		for x := 0; x < 120; x++ {
			img.Set(x, y, grid[y/10][x/10])
		}
	}
	return img
}

// Repost returns the picture of a media id resized to w × h and saved as
// a JPEG: a repost that is similar, not identical.
func Repost(id string, w, h, quality int) []byte {
	src := mediaPicture(id)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(x*120/w, y*90/h))
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, dst, &jpeg.Options{Quality: quality})
	return b.Bytes()
}

func (s *Site) posts(sub string) []*Post {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Post(nil), s.subs[strings.ToLower(sub)]...)
}

func cursor(id string) string { return base64.StdEncoding.EncodeToString([]byte("t3_" + id)) }

func ext(p *Post) string {
	switch {
	case p.Ext != "":
		return p.Ext
	case p.Kind == "gif":
		return "gif"
	}
	return "png"
}

func contentHref(p *Post) string {
	switch p.Kind {
	case "image", "gif":
		return "https://i.redd.it/" + p.Media[0] + "." + ext(p)
	case "video":
		return "https://v.redd.it/" + p.Media[0]
	case "gallery":
		return "https://www.reddit.com/gallery/" + p.ID
	case "link":
		return p.Link
	}
	return permalink(p)
}

func permalink(p *Post) string {
	slug := p.Slug
	if slug == "" {
		slug = "post"
	}
	return "/r/" + p.Subreddit + "/comments/" + p.ID + "/" + slug + "/"
}

func postHTML(p *Post, full bool) string {
	var b strings.Builder
	kind := p.Kind
	href := contentHref(p)
	if p.Original != nil {
		kind, href = "crosspost", permalink(p.Original)
	}
	fmt.Fprintf(&b, `<shreddit-post id="t3_%s" permalink="%s" post-title="%s" author="%s" subreddit-prefixed-name="r/%s" `+
		`created-timestamp="%s" post-type="%s" content-href="%s" score="%d"`, p.ID, permalink(p), html.EscapeString(p.Title), p.Author,
		p.Subreddit, p.Created.UTC().Format("2006-01-02T15:04:05.000000+0000"), kind, html.EscapeString(href), p.Score)
	if p.NSFW {
		b.WriteString(` nsfw=""`)
	}
	b.WriteString(">")
	if p.Original != nil {
		b.WriteString(postHTML(p.Original, full))
	}
	if full && p.Kind == "gallery" {
		b.WriteString("<gallery-carousel><ul>")
		for _, m := range p.Media {
			fmt.Fprintf(&b, `<li><img src="https://preview.redd.it/%s.%s?width=1080&amp;s=x">`, m, ext(p))
		}
		b.WriteString("</ul></gallery-carousel>")
	}
	if full && p.Kind == "video" {
		fmt.Fprintf(&b, `<shreddit-player-2 src="https://v.redd.it/%s/HLSPlaylist.m3u8?a=1"></shreddit-player-2>`, p.Media[0])
	}
	b.WriteString("</shreddit-post>\n")
	return b.String()
}

func (s *Site) native(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	blocked := s.nativeBlocked
	s.mu.Unlock()
	if blocked {
		w.Write([]byte("<html><body><h1>You've been blocked by network security.</h1></body></html>"))
		return
	}
	if s.checkChallenge(w, r) {
		return
	}
	parts := strings.FieldsFunc(r.URL.Path, func(c rune) bool { return c == '/' })
	switch {
	case len(parts) == 3 && parts[0] == "r" && sorts[parts[2]] && s.spelling(parts[1]) != parts[1]:
		w.Write(ShellPage("/r/" + s.spelling(parts[1]) + "/" + parts[2] + "/?" + r.URL.RawQuery))
	case len(parts) == 3 && parts[0] == "r" && sorts[parts[2]]:
		s.page(w, parts[1], parts[2], "", true)
	case len(parts) == 4 && parts[0] == "svc" && parts[2] == "community-more-posts":
		s.page(w, r.URL.Query().Get("name"), parts[3], r.URL.Query().Get("after"), false)
	case len(parts) >= 4 && parts[0] == "r" && parts[2] == "comments", len(parts) >= 2 && parts[0] == "comments":
		id := parts[1]
		if parts[0] == "r" {
			id = parts[3]
		}
		s.mu.Lock()
		p := s.byID[id]
		s.mu.Unlock()
		if p == nil {
			w.Write([]byte("<html><body>Sorry, this post was deleted.</body></html>"))
			return
		}
		if r.URL.Path != permalink(p) {
			w.Write(ShellPage(permalink(p)))
			return
		}
		fmt.Fprintf(w, "<html><body>%s</body></html>", postHTML(p, true))
	default:
		http.NotFound(w, r)
	}
}

// page writes a listing page (first) or a more-posts partial.
// sorts are the listings the fake serves: new by time, the others by
// score.
var sorts = map[string]bool{"new": true, "top": true, "hot": true, "rising": true, "controversial": true}

func (s *Site) page(w http.ResponseWriter, sub, sort, after string, first bool) {
	posts := s.posts(sub)
	if sort != "new" {
		slices.SortStableFunc(posts, func(a, b *Post) int { return b.Score - a.Score })
	}
	start := 0
	if after != "" {
		start = len(posts)
		for i, p := range posts {
			if cursor(p.ID) == after {
				start = i + 1
			}
		}
	}
	end := min(start+s.PageSize, len(posts))
	var b strings.Builder
	if first {
		s.mu.Lock()
		quarantined := s.quarantined[strings.ToLower(sub)]
		s.mu.Unlock()
		// Like Reddit's: the header has no icon attribute; the page data
		// has the spelling, the icon and the flags.
		fmt.Fprintf(&b, `<html><body><reddit-page-data data="{&quot;subreddit&quot;:{&quot;name&quot;:&quot;%s&quot;,`+
			`&quot;prefixedName&quot;:&quot;r/%[1]s&quot;,&quot;isNsfw&quot;:false,&quot;isQuarantined&quot;:%v,`+
			`&quot;communityIcon&quot;:&quot;https://styles.redditmedia.com/t5_1/styles/communityIcon_%[1]s.png&quot;}}"></reddit-page-data>`,
			sub, quarantined)
		if quarantined {
			b.WriteString(`<div>Quarantined community</div></body></html>`)
			w.Write([]byte(b.String()))
			return
		}
		fmt.Fprintf(&b, `<shreddit-subreddit-header display-name="%s" prefixed-name="r/%[1]s" icon-size="large"></shreddit-subreddit-header><shreddit-feed>`, sub)
	}
	for _, p := range posts[start:end] {
		b.WriteString("<article>" + postHTML(p, false) + "</article>")
	}
	if end < len(posts) && end > 0 {
		fmt.Fprintf(&b, `<faceplate-partial loading="lazy" src="/svc/shreddit/community-more-posts/%s/?after=%s&amp;name=%s&amp;feedViewType=compactView"></faceplate-partial>`,
			sort, cursor(posts[end-1].ID), sub)
	}
	if first {
		b.WriteString("</shreddit-feed></body></html>")
	}
	w.Write([]byte(b.String()))
}

func apiPost(p *Post) map[string]any {
	m := map[string]any{"id": p.ID, "subreddit": p.Subreddit, "author": p.Author, "title": p.Title,
		"created_utc": p.Created.Unix(), "permalink": permalink(p), "over_18": p.NSFW, "url": contentHref(p)}
	switch p.Kind {
	case "image", "gif":
		m["post_hint"] = "image"
	case "video":
		m["is_video"] = true
		m["secure_media"] = map[string]any{"reddit_video": map[string]any{
			"fallback_url": "https://v.redd.it/" + p.Media[0] + "/CMAF_720.mp4", "duration": 30}}
	case "gallery":
		m["is_gallery"] = true
		var items []map[string]any
		meta := map[string]any{}
		for _, id := range p.Media {
			items = append(items, map[string]any{"media_id": id})
			mime := map[string]string{"png": "image/png", "gif": "image/gif", "jpg": "image/jpg", "jpeg": "image/jpg"}[ext(p)]
			s := map[string]any{"x": 120, "y": 90}
			if !p.NoPreview {
				s["u"] = "https://preview.redd.it/" + id + "." + ext(p) + "?width=120&amp;s=x"
			}
			meta[id] = map[string]any{"status": "valid", "e": "Image", "m": mime, "s": s}
		}
		m["gallery_data"] = map[string]any{"items": items}
		m["media_metadata"] = meta
	case "text":
		m["is_self"] = true
	case "link":
		m["post_hint"] = "link"
	}
	if p.Original != nil {
		m["crosspost_parent"] = "t3_" + p.Original.ID
		m["crosspost_parent_list"] = []any{apiPost(p.Original)}
		m["url"] = permalink(p.Original)
	}
	return m
}

func (s *Site) arctic(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	down, maintenance := s.arcticDown, s.arcticMaintenance > 0
	if maintenance {
		s.arcticMaintenance--
	}
	s.mu.Unlock()
	if down {
		w.WriteHeader(500)
		return
	}
	if maintenance {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"data":null,"error":"Under maintenance"}`))
		return
	}
	q := r.URL.Query()
	var out []map[string]any
	switch r.URL.Path {
	case "/api/posts/search":
		limit, _ := strconv.Atoi(q.Get("limit"))
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
		for _, p := range s.posts(q.Get("subreddit")) { // newest first
			ts := p.Created.Unix()
			if (before > 0 && ts >= before) || (after > 0 && ts <= after) {
				continue
			}
			if len(out) >= limit {
				break
			}
			out = append(out, apiPost(p))
		}
	case "/api/posts/ids":
		s.mu.Lock()
		for _, id := range strings.Split(q.Get("ids"), ",") {
			if p := s.byID[id]; p != nil {
				out = append(out, apiPost(p))
			}
		}
		s.mu.Unlock()
	default:
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"data": out})
}

// video answers the manifests of the site's v.redd.it videos.
func (s *Site) video(w http.ResponseWriter, r *http.Request) {
	id, file, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	s.mu.Lock()
	known := false
	for _, p := range s.byID {
		known = known || p.Kind == "video" && len(p.Media) > 0 && p.Media[0] == id && !p.Removed && !s.gone[id]
	}
	s.mu.Unlock()
	switch {
	case !known:
		http.Error(w, "Not Found", http.StatusNotFound)
	case file == "DASHPlaylist.mpd":
		w.Header().Set("Content-Type", "application/dash+xml")
		fmt.Fprintf(w, `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet>`+
			`<Representation id="%s" bandwidth="1000"><BaseURL>DASH_720.mp4</BaseURL></Representation></AdaptationSet></Period></MPD>`, id)
	case file == "HLSPlaylist.m3u8":
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n")
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

func (s *Site) media(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	id, fileExt, _ := strings.Cut(name, ".")
	s.mu.Lock()
	b, ok := s.images[id]
	removed, want := s.gone[id], ""
	for _, p := range s.byID {
		for _, m := range p.Media {
			if m == id {
				removed, want = removed || p.Removed, ext(p)
			}
		}
	}
	s.mu.Unlock()
	// Like i.redd.it: a removed file, or one asked for under another
	// extension, is a 404 with a placeholder image.
	if removed || want != "" && fileExt != want {
		w.WriteHeader(http.StatusNotFound)
		w.Write(PNG(0))
		return
	}
	if !ok {
		b = MediaBytesExt(id, fileExt)
	}
	w.Write(b)
}
