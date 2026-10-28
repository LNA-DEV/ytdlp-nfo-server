package ytdlp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Listing is the parsed output of a subscription listing (ListArgs): the
// metadata of the channel tab or playlist and its entries in listing order.
type Listing struct {
	ID           string
	Title        string
	Channel      string
	ChannelID    string
	ChannelURL   string
	Uploader     string
	UploaderID   string
	ExtractorKey string
	WebpageURL   string
	Thumbnail    string // the channel avatar if reported, else the first thumbnail
	Count        int    // playlist_count if the site reports it, else 0
	IsPlaylist   bool
	Entries      []ListEntry
}

// ListEntry is one entry of a flat listing. Only fields of the listing are
// used; missing ones stay empty (subscriptions concept, Appendix A).
type ListEntry struct {
	URL          string
	ExtractorKey string
	ID           string
	Title        string
	Duration     float64
	Timestamp    int64  // unix seconds, 0 if unknown
	UploadDate   string // YYYYMMDD, may be approximate
	Thumbnail    string
	LiveStatus   string
	Availability string
	// Nested marks an entry that is itself a listing (a tab or playlist).
	Nested bool
	// Reddit listings (Reddit concept §7.1): the post's media kind ("" =
	// unknown), NSFW flag, and the target of a link post.
	MediaKind string
	NSFW      bool
	Link      string
	// AltKey is another key the same entry may be listed under: a Reddit
	// post's key when the entry is keyed by its media.
	AltKey string
}

// Key returns the archive key of the entry ("" if the listing has no
// extractor key or id).
func (e ListEntry) Key() string { return ArchiveKey(e.ExtractorKey, e.ID) }

func num(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case json.Number:
		f, _ := v.Float64()
		return f
	case float64:
		return v
	}
	return 0
}

// thumbnailOf returns the avatar (avatar_uncropped) if present, else the
// largest listed thumbnail, else "thumbnail".
func thumbnailOf(m map[string]any, avatar bool) string {
	th, _ := m["thumbnails"].([]any)
	if avatar {
		for _, x := range th {
			if t, ok := x.(map[string]any); ok && s(t, "id") == "avatar_uncropped" {
				return s(t, "url")
			}
		}
	}
	best, bestW := "", -1.0
	for _, x := range th {
		t, ok := x.(map[string]any)
		if !ok || !isHTTP(s(t, "url")) {
			continue
		}
		if w := num(t, "width"); w > bestW {
			best, bestW = s(t, "url"), w
		}
	}
	if best != "" {
		return best
	}
	return s(m, "thumbnail")
}

// ParseListing parses the output of ListArgs. A single video is returned
// as a non-playlist listing with the video as its only entry.
func ParseListing(data []byte) (*Listing, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse yt-dlp output: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("yt-dlp returned no metadata")
	}
	l := &Listing{
		ID:           s(m, "id"),
		Title:        s(m, "title", "playlist_title"),
		Channel:      s(m, "channel"),
		ChannelID:    s(m, "channel_id"),
		ChannelURL:   s(m, "channel_url", "uploader_url"),
		Uploader:     s(m, "uploader", "channel"),
		UploaderID:   s(m, "uploader_id"),
		ExtractorKey: s(m, "extractor_key", "ie_key"),
		WebpageURL:   s(m, "webpage_url", "original_url"),
		Count:        int(num(m, "playlist_count")),
	}
	if n, ok := m["id"].(json.Number); ok && l.ID == "" {
		l.ID = n.String()
	}
	t := s(m, "_type")
	if _, hasEntries := m["entries"]; !hasEntries && t != "playlist" && t != "multi_video" {
		l.Thumbnail = thumbnailOf(m, false)
		l.Entries = []ListEntry{entryOf(m)}
		if l.Entries[0].URL == "" {
			l.Entries[0].URL = l.WebpageURL
		}
		return l, nil
	}
	l.IsPlaylist = true
	l.Thumbnail = thumbnailOf(m, true)
	entries, _ := m["entries"].([]any)
	for _, raw := range entries {
		e, ok := raw.(map[string]any)
		if !ok || e == nil {
			continue
		}
		l.Entries = append(l.Entries, entryOf(e))
	}
	return l, nil
}

// EntryURL returns the page URL of a playlist entry. A flat entry
// (_type "url") names its page in "url". A complete entry, which some
// extractors return for every video of a page, has the media file of its
// chosen format in "url": its page is "webpage_url", which keeps the
// site's extractor and archive key instead of the generic one.
func EntryURL(e map[string]any) string {
	if t := s(e, "_type"); t != "url" && t != "url_transparent" {
		if w := s(e, "webpage_url"); isHTTP(w) {
			return w
		}
	}
	if u := s(e, "url"); isHTTP(u) {
		return u
	}
	return s(e, "webpage_url", "original_url")
}

func entryOf(e map[string]any) ListEntry {
	u := EntryURL(e)
	id := s(e, "id")
	if n, ok := e["id"].(json.Number); ok && id == "" {
		id = n.String()
	}
	le := ListEntry{
		URL:          u,
		ExtractorKey: s(e, "ie_key", "extractor_key"),
		ID:           id,
		Title:        s(e, "title", "fulltitle"),
		Duration:     num(e, "duration"),
		Timestamp:    int64(num(e, "timestamp")),
		UploadDate:   s(e, "upload_date"),
		Thumbnail:    thumbnailOf(e, false),
		LiveStatus:   s(e, "live_status"),
		Availability: s(e, "availability"),
	}
	if _, has := e["entries"]; has || nestedPlaylist(e) {
		le.Nested = true
	}
	if le.Timestamp == 0 {
		le.Timestamp = int64(num(e, "release_timestamp"))
	}
	if len(le.UploadDate) != 8 || strings.Trim(le.UploadDate, "0123456789") != "" {
		le.UploadDate = ""
	}
	return le
}
