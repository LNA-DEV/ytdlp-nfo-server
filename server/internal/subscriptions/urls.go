// Package subscriptions implements the subscription check loop: listing
// sources, diffing them against the stored entries, filters and the
// auto-queue rules (subscriptions concept §5). It never touches the library
// or the archive: everything it decides is committed by the manager as
// entries and ordinary jobs.
package subscriptions

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// YouTube channel tabs that are listed newest first (§5.2).
const (
	TabVideos  = "videos"
	TabShorts  = "shorts"
	TabStreams = "streams" // "Live" in the UI
)

// Tabs lists the tabs the add dialog offers, in UI order.
var Tabs = []string{TabVideos, TabShorts, TabStreams}

var channelIDRe = regexp.MustCompile(`^UC[0-9A-Za-z_-]{22}$`)

// IsChannelID reports whether s looks like a YouTube channel ID.
func IsChannelID(s string) bool { return channelIDRe.MatchString(s) }

func isYouTubeHost(host string) bool {
	switch strings.TrimPrefix(strings.ToLower(host), "www.") {
	case "youtube.com", "m.youtube.com", "music.youtube.com":
		return true
	}
	return false
}

// YouTubeChannel splits a YouTube channel URL (/@name, /channel/…, /c/…,
// /user/…) into its base URL and tab. The tab is "" for the channel root
// (or /featured); ok is false for other URLs, including other channel pages
// such as /playlists.
func YouTubeChannel(raw string) (base, tab string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || !isYouTubeHost(u.Hostname()) {
		return "", "", false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	n := 0
	switch {
	case len(segs) >= 1 && strings.HasPrefix(segs[0], "@") && len(segs[0]) > 1:
		n = 1
	case len(segs) >= 2 && (segs[0] == "channel" || segs[0] == "c" || segs[0] == "user") && segs[1] != "":
		n = 2
	default:
		return "", "", false
	}
	base = "https://www.youtube.com/" + strings.Join(segs[:n], "/")
	switch rest := segs[n:]; {
	case len(rest) == 0, len(rest) == 1 && rest[0] == "featured":
		return base, "", true
	case len(rest) == 1 && (rest[0] == TabVideos || rest[0] == TabShorts || rest[0] == TabStreams):
		return base, rest[0], true
	}
	return "", "", false
}

// TabURL returns the URL of a channel tab.
func TabURL(base, tab string) string { return base + "/" + tab }

// ChannelTabURL returns the canonical URL of a tab of the channel id.
func ChannelTabURL(channelID, tab string) string {
	return "https://www.youtube.com/channel/" + channelID + "/" + tab
}

// Canonical returns the canonical URL and the tab of a source (§4.1). A
// YouTube channel tab is identified by its channel ID, from the listing if
// there is one, so "…/@name/videos" and "…/channel/UC…/videos" are the same
// source and a renamed handle doesn't break the subscription. Other URLs
// are normalized like job URLs.
func Canonical(raw string, l *ytdlp.Listing) (canonical, tab string) {
	if t, ok := reddit.Parse(raw); ok && t.IsListing() {
		return t.Canonical(), "" // Reddit concept §7.1
	}
	base, tab, ok := YouTubeChannel(raw)
	if !ok || tab == "" {
		return config.NormalizeURL(raw), ""
	}
	id := ""
	if l != nil {
		switch {
		case IsChannelID(l.ChannelID):
			id = l.ChannelID
		case IsChannelID(l.ID):
			id = l.ID
		}
	}
	if id == "" {
		if rest, ok := strings.CutPrefix(base, "https://www.youtube.com/channel/"); ok && IsChannelID(rest) {
			id = rest
		}
	}
	if id != "" {
		return ChannelTabURL(id, tab), tab
	}
	return config.NormalizeURL(raw), tab
}

// NewSource returns a new source. A Reddit listing of new posts lists
// newest first, known up front, so checks use the window from the start
// (Reddit concept §7.1).
func NewSource(u, canonical, tab string) *store.Source {
	src := &store.Source{URL: u, Canonical: canonical, Tab: tab}
	if t, ok := reddit.Parse(canonical); ok && t.IsListing() && t.EffectiveSort() == reddit.SortNew {
		src.NewestFirst, src.OrderKnown = true, true
	}
	return src
}

// Kind returns "channel" if the listing is a channel (its id is the
// channel or uploader id, or, outside YouTube, its extractor is not a
// playlist-like one), else "playlist".
func Kind(l *ytdlp.Listing) string {
	if l.ID != "" && (l.ID == l.ChannelID || l.ID == l.UploaderID) {
		return "channel"
	}
	if l.IsPlaylist && l.ExtractorKey != "" && !strings.HasPrefix(strings.ToLower(l.ExtractorKey), "youtube") &&
		!playlistLike(l.ExtractorKey) {
		return "channel"
	}
	return "playlist"
}

// DisplayName is the default subscription name: the channel for a channel,
// the title for a playlist. A listing without names is named after its URL.
func DisplayName(l *ytdlp.Listing) string {
	names := []string{l.Title, l.Uploader}
	if Kind(l) == "channel" {
		names = []string{l.Channel, l.Uploader, l.Title}
	}
	for _, s := range names {
		if s != "" {
			return s
		}
	}
	if s := nameFromURL(l.WebpageURL); s != "" {
		return s
	}
	return l.ID
}

// genericSegments are path segments of channel and playlist URLs that are
// not a name.
var genericSegments = map[string]bool{"videos": true, "shorts": true, "streams": true, "live": true,
	"featured": true, "playlists": true, "playlist": true, "uploads": true, "popular": true, "latest": true,
	"newest": true, "home": true, "about": true, "user": true, "users": true, "channel": true, "channels": true,
	"c": true, "u": true, "profile": true, "profiles": true, "member": true, "members": true, "creator": true,
	"creators": true}

// nameFromURL returns the last path segment of a URL that is a name, e.g.
// "some-user" for https://example.com/users/some-user/videos.
func nameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		s, err := url.PathUnescape(segs[i])
		if err != nil {
			s = segs[i]
		}
		if s = strings.TrimPrefix(s, "@"); s != "" && !genericSegments[strings.ToLower(s)] {
			return s
		}
	}
	return ""
}

// ListingClass rewords a classified listing failure: what is missing is
// the channel or playlist, not a video.
func ListingClass(c ytdlp.Classification) ytdlp.Classification {
	if c.Class == ytdlp.ClassPermanent && c.Reason == ytdlp.ReasonUnavailable {
		c.Reason = "the channel or playlist does not exist or was removed"
	}
	return c
}
