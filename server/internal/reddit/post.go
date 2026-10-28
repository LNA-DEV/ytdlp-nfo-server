package reddit

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Media kinds of a post (§3).
const (
	KindImage   = "image"
	KindGallery = "gallery"
	KindVideo   = "video"
	KindGIF     = "gif"
	KindLink    = "link" // media on another site
	KindText    = "text"
)

// Extractor keys of Reddit items. The archive key is "<key lowercase> <id>"
// (R2): "reddit <v.redd.it id>" is yt-dlp's own key for Reddit videos.
const (
	ExtractorPost    = "RedditPost"
	ExtractorMedia   = "RedditMedia"
	ExtractorVideo   = "Reddit"
	ExtractorGallery = "RedditGallery"
)

// Media is one image or video of a post.
type Media struct {
	URL     string `json:"url"`               // the original (i.redd.it) or the v.redd.it URL
	MediaID string `json:"mediaId,omitempty"` // i.redd.it file name without extension, or the v.redd.it id
	Mime    string `json:"mime,omitempty"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
	Caption string `json:"caption,omitempty"`
	// Key is the media key if it isn't "redditmedia <MediaID>" (an Imgur
	// image of a link post, §7.6).
	Key string `json:"key,omitempty"`
}

// MediaKey returns the media key of one image.
func (m Media) MediaKey() string {
	if m.Key != "" {
		return m.Key
	}
	return ImageMediaKey(m.MediaID)
}

// Post is the normalized record of one post (§4). Fields a provider can't
// report stay empty.
type Post struct {
	ID              string    `json:"id"`
	Subreddit       string    `json:"subreddit,omitempty"`
	Author          string    `json:"author,omitempty"`
	Title           string    `json:"title,omitempty"`
	Selftext        string    `json:"selftext,omitempty"`
	Flair           string    `json:"flair,omitempty"`
	Created         time.Time `json:"created"`
	Permalink       string    `json:"permalink,omitempty"`
	Kind            string    `json:"kind"`
	NSFW            bool      `json:"nsfw,omitempty"`
	Spoiler         bool      `json:"spoiler,omitempty"`
	Media           []Media   `json:"media,omitempty"`
	VideoID         string    `json:"videoId,omitempty"`
	Duration        float64   `json:"duration,omitempty"`
	Link            string    `json:"link,omitempty"` // the target of a link post
	Thumbnail       string    `json:"thumbnail,omitempty"`
	CrosspostParent string    `json:"crosspostParent,omitempty"`
	Removed         bool      `json:"removed,omitempty"`
	Score           int       `json:"score,omitempty"`
	Provider        string    `json:"provider,omitempty"`
}

// PostKey returns the listing key of a post whose media is unknown.
func PostKey(id string) string { return "redditpost " + id }

// MediaKey returns the archive key of the post's media (R2), or "" if the
// provider didn't report the media.
func (p *Post) MediaKey() string {
	ext, id := p.MediaKeyParts()
	if id == "" {
		return ""
	}
	return strings.ToLower(ext) + " " + id
}

// MediaKeyParts returns the extractor key and id of the media key.
func (p *Post) MediaKeyParts() (extractor, id string) {
	switch p.Kind {
	case KindVideo:
		if p.VideoID != "" {
			return ExtractorVideo, p.VideoID
		}
	case KindImage, KindGIF:
		if len(p.Media) == 1 && p.Media[0].Key != "" {
			ext, id, _ := strings.Cut(p.Media[0].Key, " ")
			return ext, id
		}
		if len(p.Media) == 1 && p.Media[0].MediaID != "" {
			return ExtractorMedia, p.Media[0].MediaID
		}
	case KindGallery:
		if h := GalleryHash(p.Media); h != "" {
			return ExtractorGallery, h
		}
	}
	return "", ""
}

// ListingKey returns the media key if known, else the post key.
func (p *Post) ListingKey() (extractor, id string) {
	if e, id := p.MediaKeyParts(); id != "" {
		return e, id
	}
	return ExtractorPost, p.ID
}

// GalleryHash returns the first 16 hex digits of the SHA-256 of the media
// IDs in gallery order, joined by commas (§7.5); "" if an ID is missing.
func GalleryHash(media []Media) string {
	if len(media) == 0 {
		return ""
	}
	ids := make([]string, len(media))
	for i, m := range media {
		if m.MediaID == "" {
			return ""
		}
		ids[i] = m.MediaID
	}
	sum := sha256.Sum256([]byte(strings.Join(ids, ",")))
	return hex.EncodeToString(sum[:])[:16]
}

// ImageMediaKey returns the media key of one image ("redditmedia <id>").
func ImageMediaKey(mediaID string) string { return "redditmedia " + mediaID }

var (
	vreddRe = regexp.MustCompile(`(?i)^https?://v\.redd\.it/([a-z0-9]+)`)
	ireddRe = regexp.MustCompile(`(?i)^https?://(?:i|preview)\.redd\.it/([a-z0-9_-]+)\.([a-z0-9]+)`)
)

// VideoIDFromURL returns the v.redd.it id of a URL, or "".
func VideoIDFromURL(u string) string {
	if m := vreddRe.FindStringSubmatch(u); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// ImageFromURL returns the media id and extension of an i.redd.it or
// preview.redd.it URL ("" if it isn't one).
func ImageFromURL(u string) (id, ext string) {
	if m := ireddRe.FindStringSubmatch(u); m != nil {
		return m[1], strings.ToLower(m[2])
	}
	return "", ""
}

// OriginalImageURL returns the i.redd.it URL of an image id and extension.
// The extension is kept as it was uploaded: i.redd.it serves a file only
// under its own extension, and "x.jpeg" is a 404 as "x.jpg".
func OriginalImageURL(id, ext string) string {
	return "https://i.redd.it/" + id + "." + strings.ToLower(ext)
}

// OtherJPEGSpelling returns an i.redd.it JPEG URL with the other spelling
// of its extension (.jpg ↔ .jpeg), or "" for any other URL: the fallback
// when only the MIME type named the extension.
func OtherJPEGSpelling(u string) string {
	switch {
	case !strings.HasPrefix(u, "https://i.redd.it/"):
		return ""
	case strings.HasSuffix(u, ".jpg"):
		return strings.TrimSuffix(u, ".jpg") + ".jpeg"
	case strings.HasSuffix(u, ".jpeg"):
		return strings.TrimSuffix(u, ".jpeg") + ".jpg"
	}
	return ""
}

// ManifestURL returns the DASH manifest of a v.redd.it video (§4).
func ManifestURL(videoID string) string { return "https://v.redd.it/" + videoID + "/DASHPlaylist.mpd" }

// HLSURL returns the HLS fallback of a v.redd.it video.
func HLSURL(videoID string) string { return "https://v.redd.it/" + videoID + "/HLSPlaylist.m3u8" }

// LinkHost returns the host of a link post's target.
func (p *Post) LinkHost() string {
	if u, err := url.Parse(p.Link); err == nil {
		return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	}
	return ""
}

// Classify sets the kind and media of a post from its content URL and,
// if the provider has one, its own type hint ("image", "gallery", "video",
// "text", "link", "crosspost", …). A link to another Reddit post is a
// crosspost: its kind stays unknown until the original is fetched.
func (p *Post) Classify(content, hint string) {
	hint = strings.ToLower(hint)
	p.Kind, p.Link, p.Media, p.VideoID = "", "", nil, ""
	switch {
	case content == "" || hint == "text" || hint == "self":
		p.Kind = KindText
	case VideoIDFromURL(content) != "":
		p.Kind, p.VideoID = KindVideo, VideoIDFromURL(content)
		p.Media = []Media{{URL: "https://v.redd.it/" + p.VideoID, MediaID: p.VideoID}}
	default:
		if id, ext := ImageFromURL(content); id != "" && strings.Contains(strings.ToLower(content), "i.redd.it") {
			p.Kind = KindImage
			if ext == "gif" {
				p.Kind = KindGIF
			}
			p.Media = []Media{{URL: OriginalImageURL(id, ext), MediaID: id, Mime: mimeOf(ext)}}
			return
		}
		u, err := url.Parse(content)
		if err != nil || u.Host == "" {
			p.Kind = KindText
			return
		}
		host := strings.ToLower(u.Hostname())
		switch {
		case IsRedditHost(host) && (strings.Contains(u.Path, "/gallery/") || hint == "gallery" || hint == "multi_media"):
			p.Kind = KindGallery
		case IsRedditHost(host):
			t, ok := Parse(content)
			if ok && t.Kind == TargetPost && t.PostID != p.ID {
				p.CrosspostParent = t.PostID // kind unknown until the original is fetched
				return
			}
			p.Kind = KindText
		default:
			p.Kind, p.Link = KindLink, content
		}
	}
	if hint == "gallery" && p.Kind == KindText {
		p.Kind = KindGallery
	}
}

func mimeOf(ext string) string {
	switch ext {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "avif":
		return "image/avif"
	}
	return ""
}
