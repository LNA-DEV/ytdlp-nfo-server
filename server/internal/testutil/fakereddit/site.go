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
}

// NewSite starts a fake site.
func NewSite(t testing.TB) *Site {
	s := &Site{Server: New(t), subs: map[string][]*Post{}, byID: map[string]*Post{}, PageSize: 25, images: map[string][]byte{}, gone: map[string]bool{}}
	s.Handle("www.reddit.com", s.native)
	s.Handle("reddit.com", s.native)
	s.Handle("arctic-shift.photon-reddit.com", s.arctic)
	s.Handle("i.redd.it", s.media)
	return s
}

// Add adds posts (newest first after sorting by time).
func (s *Site) Add(posts ...*Post) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range posts {
		sub := strings.ToLower(p.Subreddit)
		s.subs[sub] = append(s.subs[sub], p)
		s.byID[p.ID] = p
		if p.Original != nil {
			s.byID[p.Original.ID] = p.Original
		}
		sort.SliceStable(s.subs[sub], func(i, j int) bool { return s.subs[sub][i].Created.After(s.subs[sub][j].Created) })
	}
}

// SetNativeBlocked makes native pages answer with the block page.
func (s *Site) SetNativeBlocked(b bool) { s.mu.Lock(); s.nativeBlocked = b; s.mu.Unlock() }

// SetArcticDown makes the Arctic Shift API answer 500.
func (s *Site) SetArcticDown(b bool) { s.mu.Lock(); s.arcticDown = b; s.mu.Unlock() }

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
	var b bytes.Buffer
	img := mediaPicture(id)
	if gifImage {
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
	if p.Kind == "gif" {
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

func permalink(p *Post) string { return "/r/" + p.Subreddit + "/comments/" + p.ID + "/post/" }

func postHTML(p *Post, full bool) string {
	var b strings.Builder
	kind := p.Kind
	href := contentHref(p)
	if p.Original != nil {
		kind, href = "crosspost", permalink(p.Original)
	}
	fmt.Fprintf(&b, `<shreddit-post id="t3_%s" permalink="%s" post-title="%s" author="%s" subreddit-prefixed-name="r/%s" `+
		`created-timestamp="%s" post-type="%s" content-href="%s"`, p.ID, permalink(p), html.EscapeString(p.Title), p.Author,
		p.Subreddit, p.Created.UTC().Format("2006-01-02T15:04:05.000000+0000"), kind, html.EscapeString(href))
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
			fmt.Fprintf(&b, `<li><img src="https://preview.redd.it/%s.png?width=1080&amp;s=x">`, m)
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
	parts := strings.FieldsFunc(r.URL.Path, func(c rune) bool { return c == '/' })
	switch {
	case len(parts) == 3 && parts[0] == "r" && parts[2] == "new":
		s.page(w, parts[1], "", true)
	case len(parts) == 4 && parts[0] == "svc" && parts[2] == "community-more-posts":
		s.page(w, r.URL.Query().Get("name"), r.URL.Query().Get("after"), false)
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
		fmt.Fprintf(w, "<html><body>%s</body></html>", postHTML(p, true))
	default:
		http.NotFound(w, r)
	}
}

// page writes a listing page (first) or a more-posts partial.
func (s *Site) page(w http.ResponseWriter, sub, after string, first bool) {
	posts := s.posts(sub)
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
		fmt.Fprintf(&b, `<html><body><shreddit-subreddit-header display-name="%s"></shreddit-subreddit-header><shreddit-feed>`, sub)
	}
	for _, p := range posts[start:end] {
		b.WriteString("<article>" + postHTML(p, false) + "</article>")
	}
	if end < len(posts) && end > 0 {
		fmt.Fprintf(&b, `<faceplate-partial loading="lazy" src="/svc/shreddit/community-more-posts/new/?after=%s&amp;name=%s&amp;feedViewType=compactView"></faceplate-partial>`,
			cursor(posts[end-1].ID), sub)
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
			meta[id] = map[string]any{"status": "valid", "e": "Image", "m": "image/png", "s": map[string]any{"x": 80, "y": 60}}
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
	down := s.arcticDown
	s.mu.Unlock()
	if down {
		w.WriteHeader(500)
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

func (s *Site) media(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	id := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, ".png"), ".gif"), ".jpg")
	s.mu.Lock()
	b, ok := s.images[id]
	removed := s.gone[id]
	for _, p := range s.byID {
		for _, m := range p.Media {
			if m == id && p.Removed {
				removed = true
			}
		}
	}
	s.mu.Unlock()
	if removed {
		http.NotFound(w, r)
		return
	}
	if !ok {
		b = MediaBytes(id, strings.HasSuffix(name, ".gif"))
	}
	w.Write(b)
}
