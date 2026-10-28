package ytdlp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Entry is one video found by a probe.
type Entry struct {
	URL          string
	ExtractorKey string
	ID           string
	Title        string
	Uploader     string
	// Nested marks an entry that is itself a playlist (e.g. a channel tab)
	// that --flat-playlist did not expand; it must be probed separately.
	Nested bool
}

// ArchiveKey returns yt-dlp's archive key "<extractor_key lowercase> <id>".
func ArchiveKey(extractorKey, id string) string {
	if extractorKey == "" || id == "" {
		return ""
	}
	return strings.ToLower(extractorKey) + " " + id
}

// Key returns the archive key of the entry ("" if unknown).
func (e Entry) Key() string { return ArchiveKey(e.ExtractorKey, e.ID) }

// ProbeResult is the parsed output of a --flat-playlist -J probe.
type ProbeResult struct {
	IsPlaylist bool
	Title      string
	Uploader   string
	Thumbnail  string
	Entries    []Entry
	// Single holds the full metadata of a single video. The probe of a
	// single video is a complete extraction, so it is used as info.json
	// directly (the video is extracted only once).
	Single []byte
}

func s(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func isHTTP(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// nestedPlaylist guesses whether a flat "url" entry points to another
// playlist (channel tabs, playlists of playlists).
func nestedPlaylist(e map[string]any) bool {
	if t := s(e, "_type"); t == "playlist" || t == "multi_video" {
		return true
	}
	ie := strings.ToLower(s(e, "ie_key"))
	for _, suffix := range []string{"tab", "playlist", "channel", "user", "series", "season", "show"} {
		if strings.HasSuffix(ie, suffix) {
			return true
		}
	}
	return false
}

// ParseProbe parses probe JSON.
func ParseProbe(data []byte) (*ProbeResult, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse yt-dlp output: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("yt-dlp returned no metadata")
	}
	pr := &ProbeResult{
		Title:     s(m, "title", "playlist_title", "fulltitle"),
		Uploader:  s(m, "uploader", "channel", "uploader_id"),
		Thumbnail: s(m, "thumbnail"),
	}
	t := s(m, "_type")
	if _, hasEntries := m["entries"]; hasEntries || t == "playlist" || t == "multi_video" {
		pr.IsPlaylist = true
		flatten(m, &pr.Entries, 0)
		if pr.Thumbnail == "" {
			if th, ok := m["thumbnails"].([]any); ok && len(th) > 0 {
				if last, ok := th[len(th)-1].(map[string]any); ok {
					pr.Thumbnail = s(last, "url")
				}
			}
		}
		return pr, nil
	}
	e := Entry{
		URL:          s(m, "webpage_url", "original_url"),
		ExtractorKey: s(m, "extractor_key"),
		ID:           s(m, "id"),
		Title:        s(m, "title", "fulltitle"),
		Uploader:     pr.Uploader,
	}
	if id, ok := m["id"].(json.Number); ok && e.ID == "" {
		e.ID = id.String()
	}
	pr.Entries = []Entry{e}
	pr.Single = data
	return pr, nil
}

// flatten collects video entries, recursing into inline nested playlists
// (same semantics as _flatten_entries in ytdlp-nfo).
func flatten(m map[string]any, out *[]Entry, depth int) {
	entries, _ := m["entries"].([]any)
	for _, raw := range entries {
		e, ok := raw.(map[string]any)
		if !ok || e == nil {
			continue
		}
		if _, has := e["entries"]; has && depth < 5 {
			flatten(e, out, depth+1)
			continue
		}
		u := EntryURL(e)
		id := s(e, "id")
		if n, ok := e["id"].(json.Number); ok && id == "" {
			id = n.String()
		}
		*out = append(*out, Entry{
			URL:          u,
			ExtractorKey: s(e, "ie_key", "extractor_key"),
			ID:           id,
			Title:        s(e, "title"),
			Uploader:     s(e, "uploader", "channel"),
			Nested:       nestedPlaylist(e),
		})
	}
}

// Meta is the subset of info.json fields the pipeline needs.
type Meta struct {
	Raw          map[string]any
	ID           string
	ExtractorKey string
	Extractor    string
	Title        string
	Uploader     string
	Channel      string
	UploadDate   string
	Duration     float64
	WebpageURL   string
	IsPlaylist   bool
	Formats      []map[string]any
	Subtitles    map[string]any
	Thumbnails   []map[string]any
	Thumbnail    string
	HTTPHeaders  map[string]string
	FilesizeHint int64
}

// ParseMeta parses an info.json document.
func ParseMeta(data []byte) (*Meta, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse info.json: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("yt-dlp returned no metadata")
	}
	md := &Meta{
		Raw:          m,
		ID:           s(m, "id"),
		ExtractorKey: s(m, "extractor_key"),
		Extractor:    s(m, "extractor"),
		Title:        s(m, "title", "fulltitle"),
		Uploader:     s(m, "uploader", "channel"),
		Channel:      s(m, "channel", "uploader"),
		UploadDate:   s(m, "upload_date"),
		WebpageURL:   s(m, "webpage_url", "original_url"),
		Thumbnail:    s(m, "thumbnail"),
	}
	if id, ok := m["id"].(float64); ok && md.ID == "" {
		md.ID = fmt.Sprintf("%.0f", id)
	}
	if _, ok := m["entries"]; ok || s(m, "_type") == "playlist" {
		md.IsPlaylist = true
	}
	if d, ok := m["duration"].(float64); ok {
		md.Duration = d
	}
	md.Formats = anyList(m["formats"])
	md.Thumbnails = anyList(m["thumbnails"])
	md.Subtitles, _ = m["subtitles"].(map[string]any)
	if h, ok := m["http_headers"].(map[string]any); ok {
		md.HTTPHeaders = map[string]string{}
		for k, v := range h {
			if vs, ok := v.(string); ok {
				md.HTTPHeaders[k] = vs
			}
		}
	}
	md.FilesizeHint = estimateSize(m)
	return md, nil
}

func anyList(v any) []map[string]any {
	l, _ := v.([]any)
	out := make([]map[string]any, 0, len(l))
	for _, x := range l {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func size(m map[string]any) int64 {
	for _, k := range []string{"filesize", "filesize_approx"} {
		if f, ok := m[k].(float64); ok && f > 0 {
			return int64(f)
		}
	}
	return 0
}

// estimateSize sums the sizes of the formats yt-dlp selected by default
// (plus every audio-only format, a safe upper bound for all-audio mode).
func estimateSize(m map[string]any) int64 {
	var total int64
	if rf := anyList(m["requested_formats"]); len(rf) > 0 {
		for _, f := range rf {
			total += size(f)
		}
	} else {
		total = size(m)
	}
	for _, f := range anyList(m["formats"]) {
		if s(f, "vcodec") == "none" && s(f, "acodec") != "none" && s(f, "language") != "" {
			total += size(f) / 2 // other language tracks; rough
		}
	}
	return total
}

// SubtitleLanguages returns the requested subtitle languages (live_chat
// excluded, F18).
func (m *Meta) SubtitleLanguages() []string {
	var langs []string
	for k := range m.Subtitles {
		if k != "live_chat" {
			langs = append(langs, k)
		}
	}
	return langs
}
