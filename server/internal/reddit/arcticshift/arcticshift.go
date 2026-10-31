// Package arcticshift reads Reddit posts from the Arctic Shift API, a
// third-party archive (Reddit concept §6). It lists by time only: it can't
// rank by score, and its scores settle after about 36 hours.
package arcticshift

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// BaseURL is the API's base URL.
const BaseURL = "https://arctic-shift.photon-reddit.com"

// maxResponse limits one API response.
const maxResponse = 16 << 20

// Provider is the Arctic Shift provider.
type Provider struct {
	Client *browser.Client
	// MinInterval is the pause between two requests (§6: at least 1 s, as
	// the operator asks). Tests set it to zero.
	MinInterval time.Duration

	mu   sync.Mutex
	last time.Time
}

// New creates the provider.
func New(c *browser.Client) *Provider { return &Provider{Client: c, MinInterval: time.Second} }

// Name implements reddit.Provider.
func (p *Provider) Name() string { return reddit.ProviderArcticShift }

// Site implements reddit.Provider.
func (p *Provider) Site() string { return "arctic-shift" }

// pace waits until gap has passed since the last request. Requests of one
// provider run one at a time.
func (p *Provider) pace(ctx context.Context, gap time.Duration) error {
	p.mu.Lock()
	wait := time.Until(p.last.Add(gap))
	if wait < 0 {
		wait = 0
	}
	p.last = time.Now().Add(wait)
	p.mu.Unlock()
	if wait == 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type response struct {
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

func (p *Provider) get(ctx context.Context, path string, q url.Values) ([]apiPost, error) {
	raw, err := p.getRaw(ctx, path, q, p.MinInterval)
	if err != nil {
		return nil, err
	}
	var posts []apiPost
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &posts); err != nil {
			return nil, &reddit.Error{Class: ytdlp.ClassUnknown, Reason: "Arctic Shift: unreadable answer: " + err.Error()}
		}
	}
	return posts, nil
}

func (p *Provider) getRaw(ctx context.Context, path string, q url.Values, gap time.Duration) (json.RawMessage, error) {
	if err := p.pace(ctx, gap); err != nil {
		return nil, err
	}
	resp, err := p.Client.JSON(ctx, BaseURL+path+"?"+q.Encode(), maxResponse)
	if err != nil {
		return nil, &reddit.Error{Class: ytdlp.ClassTransient, Reason: "Arctic Shift: " + err.Error()}
	}
	var r response
	_ = json.Unmarshal(resp.Body, &r)
	switch {
	case resp.Status == 429:
		e := &reddit.Error{Class: ytdlp.ClassRateLimited, Reason: "rate-limited by Arctic Shift"}
		if n, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Reset")); err == nil && n > 0 {
			e.RetryAfter = time.Duration(n) * time.Second
		}
		return nil, e
	case resp.Status == 422 || strings.Contains(strings.ToLower(r.Error), "slow down"):
		// "Timeout. Maybe slow down a bit": the API is overloaded by us.
		e := &reddit.Error{Class: ytdlp.ClassRateLimited, Reason: "Arctic Shift asks to slow down: " + r.Error}
		if n, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Reset")); err == nil && n > 0 {
			e.RetryAfter = time.Duration(n) * time.Second
		}
		return nil, e
	case strings.Contains(strings.ToLower(r.Error), "timed out"):
		return nil, &reddit.Error{Class: ytdlp.ClassTransient, Reason: "Arctic Shift: query timed out"}
	case resp.Status >= 500:
		return nil, &reddit.Error{Class: ytdlp.ClassTransient, Reason: fmt.Sprintf("Arctic Shift: HTTP %d", resp.Status)}
	case resp.Status != 200:
		return nil, &reddit.Error{Class: ytdlp.ClassUnknown, Reason: fmt.Sprintf("Arctic Shift: HTTP %d %s", resp.Status, r.Error)}
	case r.Error != "":
		return nil, &reddit.Error{Class: ytdlp.ClassUnknown, Reason: "Arctic Shift: " + r.Error}
	}
	return r.Data, nil
}

// List implements reddit.Provider: newest first, paged by time.
func (p *Provider) List(ctx context.Context, q reddit.Query) (*reddit.Page, error) {
	t := q.Target
	if !t.IsListing() || t.EffectiveSort() != reddit.SortNew {
		return nil, &reddit.Error{Class: ytdlp.ClassPermanent, Reason: "Arctic Shift only lists posts by time", Hint: reddit.HintTopNative,
			Unsupported: true}
	}
	limit := max(q.Limit, 1)
	out := &reddit.Page{Title: t.Name()}
	seen := map[string]bool{}
	before := q.Before
	for len(out.Posts) < limit {
		// One more than needed: the post at the boundary comes again.
		size := min(100, limit-len(out.Posts)+1)
		v := url.Values{"sort": {"desc"}, "limit": {strconv.Itoa(size)}}
		if t.Kind == reddit.TargetSubreddit {
			v.Set("subreddit", t.Subreddit)
		} else {
			v.Set("author", t.User)
		}
		if !q.After.IsZero() {
			v.Set("after", strconv.FormatInt(q.After.Unix(), 10))
		}
		if !before.IsZero() {
			v.Set("before", strconv.FormatInt(before.Unix()+1, 10)) // inclusive: posts of the same second
		}
		filters(v, q)
		data, err := p.get(ctx, "/api/posts/search", v)
		if err != nil {
			return nil, err
		}
		added := 0
		for i := range data {
			ps := data[i].post()
			if ps.ID == "" || seen[ps.ID] {
				continue
			}
			seen[ps.ID] = true
			out.Posts = append(out.Posts, ps)
			added++
			if before.IsZero() || ps.Created.Before(before) {
				before = ps.Created
			}
		}
		if added == 0 || len(data) < size {
			out.End = true
			break
		}
	}
	if len(out.Posts) > limit {
		out.Posts = out.Posts[:limit]
	}
	return out, nil
}

// filters adds the server-side filters of a query (§7.7).
func filters(v url.Values, q reddit.Query) {
	if q.URLPrefix != "" {
		v.Set("url", q.URLPrefix)
	}
	if q.Over18 != nil {
		v.Set("over_18", strconv.FormatBool(*q.Over18))
	}
}

// Count implements reddit.Counter: the posts of a time range per year,
// from the aggregation endpoint (§7.7). Its answer is not documented, so
// the common shapes are read; anything else counts as unknown. The
// endpoint needs one of the bounds: an open range ends a day from now,
// clear of the exclusive bound and of clock skew.
func (p *Provider) Count(ctx context.Context, q reddit.Query) ([]reddit.YearCount, error) {
	t := q.Target
	if !t.IsListing() {
		return nil, &reddit.Error{Class: ytdlp.ClassPermanent, Reason: "not a listing", Unsupported: true}
	}
	v := url.Values{"aggregate": {"created_utc"}, "frequency": {"year"}}
	if t.Kind == reddit.TargetSubreddit {
		v.Set("subreddit", t.Subreddit)
	} else {
		v.Set("author", t.User)
	}
	if !q.After.IsZero() {
		v.Set("after", strconv.FormatInt(q.After.Unix(), 10))
	}
	before := q.Before
	if before.IsZero() && q.After.IsZero() {
		before = time.Now().Add(24 * time.Hour)
	}
	if !before.IsZero() {
		v.Set("before", strconv.FormatInt(before.Unix(), 10))
	}
	filters(v, q)
	// A count is a heavy query: a longer pause before it (several in a
	// row made the API ask to slow down).
	raw, err := p.getRaw(ctx, "/api/posts/search/aggregate", v, 5*p.MinInterval)
	if err != nil {
		return nil, err
	}
	counts, err := parseCounts(raw)
	if err != nil {
		return nil, &reddit.Error{Class: ytdlp.ClassUnknown, Reason: "Arctic Shift: " + err.Error()}
	}
	return counts, nil
}

// parseCounts reads time buckets: a list of {created_utc|key|date, count|
// doc_count}, or such a list under "created_utc". The count may be a
// number or a numeric string. A bucket starts at the year's start in the
// API's time zone (the real one at 23:00 UTC the day before), so it
// belongs to the year of the day after its start.
func parseCounts(raw json.RawMessage) ([]reddit.YearCount, error) {
	errFormat := errors.New("unknown format of the post counts")
	var buckets []map[string]any
	if json.Unmarshal(raw, &buckets) != nil {
		var obj map[string][]map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			return nil, errFormat
		}
		buckets = obj["created_utc"]
	}
	byYear := map[int]int{}
	for _, b := range buckets {
		at, okT := bucketTime(firstOf(b, "created_utc", "key", "date", "time"))
		n, okN := bucketCount(firstOf(b, "count", "doc_count"))
		if !okT || !okN {
			return nil, errFormat
		}
		byYear[at.Add(14*time.Hour).UTC().Year()] += n
	}
	out := make([]reddit.YearCount, 0, len(byYear))
	for y, n := range byYear {
		out = append(out, reddit.YearCount{Year: y, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Year < out[j].Year })
	return out, nil
}

func firstOf(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

func bucketCount(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), x >= 0
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		return n, err == nil && n >= 0
	}
	return 0, false
}

// bucketTime reads a bucket's time: Unix seconds or milliseconds, or an
// ISO 8601 date.
func bucketTime(v any) (time.Time, bool) {
	switch x := v.(type) {
	case float64:
		if x > 1e11 {
			return time.UnixMilli(int64(x)), true
		}
		return time.Unix(int64(x), 0), true
	case string:
		if n, err := strconv.ParseFloat(x, 64); err == nil {
			return bucketTime(n)
		}
		for _, l := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02", "2006"} {
			if t, err := time.Parse(l, x); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// Post implements reddit.Provider.
func (p *Provider) Post(ctx context.Context, id, _ string) (*reddit.Post, error) {
	data, err := p.get(ctx, "/api/posts/ids", url.Values{"ids": {id}})
	if err != nil {
		return nil, err
	}
	for i := range data {
		if ps := data[i].post(); ps.ID == id {
			return &ps, nil
		}
	}
	return nil, &reddit.Error{Class: ytdlp.ClassPermanent, Reason: "post not in Arctic Shift"}
}

// apiPost is a Reddit post object as Arctic Shift stores it.
type apiPost struct {
	ID                  string               `json:"id"`
	Subreddit           string               `json:"subreddit"`
	Author              string               `json:"author"`
	Title               string               `json:"title"`
	Selftext            string               `json:"selftext"`
	LinkFlairText       string               `json:"link_flair_text"`
	CreatedUTC          flexNumber           `json:"created_utc"`
	Permalink           string               `json:"permalink"`
	URL                 string               `json:"url"`
	URLOverridden       string               `json:"url_overridden_by_dest"`
	IsSelf              bool                 `json:"is_self"`
	IsVideo             bool                 `json:"is_video"`
	IsGallery           bool                 `json:"is_gallery"`
	PostHint            string               `json:"post_hint"`
	Over18              bool                 `json:"over_18"`
	Spoiler             bool                 `json:"spoiler"`
	Score               flexNumber           `json:"score"`
	Thumbnail           string               `json:"thumbnail"`
	RemovedByCategory   *string              `json:"removed_by_category"`
	MediaMetadata       map[string]mediaMeta `json:"media_metadata"`
	GalleryData         *galleryData         `json:"gallery_data"`
	SecureMedia         *mediaObj            `json:"secure_media"`
	Media               *mediaObj            `json:"media"`
	Preview             *preview             `json:"preview"`
	CrosspostParent     string               `json:"crosspost_parent"`
	CrosspostParentList []apiPost            `json:"crosspost_parent_list"`
}

type mediaMeta struct {
	Status string `json:"status"`
	E      string `json:"e"`
	M      string `json:"m"`
	ID     string `json:"id"`
	S      struct {
		U   string     `json:"u"`
		Gif string     `json:"gif"`
		Mp4 string     `json:"mp4"`
		X   flexNumber `json:"x"`
		Y   flexNumber `json:"y"`
	} `json:"s"`
	DashURL string `json:"dashUrl"`
	HLSURL  string `json:"hlsUrl"`
}

type galleryData struct {
	Items []struct {
		MediaID string `json:"media_id"`
		Caption string `json:"caption"`
	} `json:"items"`
}

type mediaObj struct {
	RedditVideo *struct {
		FallbackURL string     `json:"fallback_url"`
		DashURL     string     `json:"dash_url"`
		HLSURL      string     `json:"hls_url"`
		Duration    flexNumber `json:"duration"`
	} `json:"reddit_video"`
}

type image struct {
	URL    string     `json:"url"`
	Width  flexNumber `json:"width"`
	Height flexNumber `json:"height"`
}

type preview struct {
	Images []struct {
		Source      image   `json:"source"`
		Resolutions []image `json:"resolutions"`
	} `json:"images"`
}

// flexNumber reads a JSON number or numeric string.
type flexNumber float64

func (f *flexNumber) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil // archived data is not always clean; ignore odd values
	}
	*f = flexNumber(v)
	return nil
}

func extFromMime(m string) string {
	switch strings.ToLower(m) {
	case "image/jpg":
		return "jpg"
	case "image/jpeg":
		return "jpeg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	}
	return "jpg"
}

// post converts the archived object (§4).
func (a *apiPost) post() reddit.Post {
	p := reddit.Post{
		ID: strings.ToLower(a.ID), Subreddit: a.Subreddit, Author: a.Author, Title: html.UnescapeString(a.Title),
		Selftext: a.Selftext, Flair: a.LinkFlairText, NSFW: a.Over18, Spoiler: a.Spoiler, Score: int(a.Score),
		Provider: reddit.ProviderArcticShift,
	}
	if a.CreatedUTC > 0 {
		p.Created = time.Unix(int64(a.CreatedUTC), 0).UTC()
	}
	if a.Permalink != "" {
		p.Permalink = "https://www.reddit.com" + a.Permalink
	} else {
		p.Permalink = reddit.PostURL(a.Subreddit, p.ID)
	}
	p.Removed = removed(a)
	src := a
	if len(a.CrosspostParentList) > 0 {
		src = &a.CrosspostParentList[0]
		p.CrosspostParent = strings.ToLower(src.ID)
	} else if a.CrosspostParent != "" {
		p.CrosspostParent = strings.TrimPrefix(strings.ToLower(a.CrosspostParent), "t3_")
	}
	p.Thumbnail = src.thumbnail()
	switch {
	case src.IsGallery || src.GalleryData != nil && len(src.GalleryData.Items) > 0:
		p.Kind = reddit.KindGallery
		if src.GalleryData != nil {
			for _, it := range src.GalleryData.Items {
				meta, ok := src.MediaMetadata[it.MediaID]
				if !ok || (meta.Status != "" && meta.Status != "valid") {
					continue
				}
				m := reddit.Media{MediaID: it.MediaID, Caption: it.Caption, Width: int(meta.S.X), Height: int(meta.S.Y)}
				switch meta.E {
				case "AnimatedImage":
					m.URL, m.Mime = reddit.OriginalImageURL(it.MediaID, "gif"), "image/gif"
				case "RedditVideo":
					vid := reddit.VideoIDFromURL(meta.DashURL)
					if vid == "" {
						vid = reddit.VideoIDFromURL(meta.HLSURL)
					}
					m.URL, m.Mime = "https://v.redd.it/"+vid, "video/mp4"
				default:
					// The extension of the preview URL is the uploaded one;
					// the MIME type can't tell .jpg from .jpeg.
					_, ext := reddit.ImageFromURL(html.UnescapeString(meta.S.U))
					if ext == "" {
						ext = extFromMime(meta.M)
					}
					m.URL, m.Mime = reddit.OriginalImageURL(it.MediaID, ext), "image/"+strings.Replace(ext, "jpg", "jpeg", 1)
				}
				p.Media = append(p.Media, m)
			}
		}
	case src.IsVideo || src.redditVideo() != "":
		vid := src.redditVideo()
		p.Kind, p.VideoID = reddit.KindVideo, vid
		if vid != "" {
			p.Media = []reddit.Media{{URL: "https://v.redd.it/" + vid, MediaID: vid}}
		}
		p.Duration = src.duration()
	default:
		content := src.URLOverridden
		if content == "" {
			content = src.URL
		}
		hint := map[string]string{"image": "image", "hosted:video": "video", "link": "link", "rich:video": "link", "self": "text"}[src.PostHint]
		if src.IsSelf {
			hint, content = "text", ""
		}
		id := p.ID
		p.ID = strings.ToLower(src.ID) // a crosspost's URL may point at its original
		p.Classify(html.UnescapeString(content), hint)
		p.ID = id
		if p.CrosspostParent != "" && p.Kind == "" {
			p.Kind = reddit.KindText
		}
	}
	return p
}

// removed reports whether a post's media was taken down. Posts filtered
// by AutoModerator or Reddit's spam filters ("reddit") are often approved
// later and keep their media, as do categories we don't know: the
// download decides for them.
func removed(a *apiPost) bool {
	if a.RemovedByCategory != nil {
		switch *a.RemovedByCategory {
		case "deleted", "author", "moderator", "anti_evil_ops", "community_ops", "copyright_takedown",
			"content_takedown", "legal":
			return true
		}
		return false
	}
	return a.Selftext == "[removed]" || a.Selftext == "[deleted]"
}

// redditVideo returns the v.redd.it id of a video post from whichever of
// its fields has it: the media objects, the post's URL or its destination.
func (a *apiPost) redditVideo() string {
	for _, m := range []*mediaObj{a.SecureMedia, a.Media} {
		if m != nil && m.RedditVideo != nil {
			for _, u := range []string{m.RedditVideo.FallbackURL, m.RedditVideo.DashURL, m.RedditVideo.HLSURL} {
				if id := reddit.VideoIDFromURL(u); id != "" {
					return id
				}
			}
		}
	}
	for _, u := range []string{a.URLOverridden, a.URL} {
		if id := reddit.VideoIDFromURL(u); id != "" {
			return id
		}
	}
	return ""
}

func (a *apiPost) duration() float64 {
	for _, m := range []*mediaObj{a.SecureMedia, a.Media} {
		if m != nil && m.RedditVideo != nil && m.RedditVideo.Duration > 0 {
			return float64(m.RedditVideo.Duration)
		}
	}
	return 0
}

// thumbnail picks a preview of about 640 px, else the largest one.
func (a *apiPost) thumbnail() string {
	if a.Preview != nil && len(a.Preview.Images) > 0 {
		img := a.Preview.Images[0]
		best := img.Source
		for _, r := range img.Resolutions {
			if r.Width >= 320 && r.Width <= 960 {
				best = r
			}
		}
		if best.URL != "" {
			return html.UnescapeString(best.URL)
		}
	}
	if strings.HasPrefix(a.Thumbnail, "https://") {
		return html.UnescapeString(a.Thumbnail)
	}
	return ""
}
