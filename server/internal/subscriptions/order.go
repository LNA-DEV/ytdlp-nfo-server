package subscriptions

import (
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// The order a source lists in (§5.2). Channel and user pages list their
// newest entry first, playlists in playlist order (usually oldest first).
// The order decides the sort dates of entries without a date and which
// entries "latest N" takes, and a source that lists newest first can be
// checked with a window. Only YouTube tabs are known up front; for other
// sources the order is guessed until the listing confirms it.

// playlistLikeRe matches extractor keys of playlists, albums, sets and
// similar listings that keep their own order.
var playlistLikeRe = regexp.MustCompile(`(?i)(playlist|album|showcase|collection|series|season|course|set$)`)

func playlistLike(extractorKey string) bool { return playlistLikeRe.MatchString(extractorKey) }

func isYouTube(src *store.Source, l *ytdlp.Listing) bool {
	if l != nil && strings.HasPrefix(strings.ToLower(l.ExtractorKey), "youtube") {
		return true
	}
	u, err := url.Parse(src.Canonical)
	return err == nil && isYouTubeHost(u.Hostname())
}

// guessNewestFirst guesses the order of a source that the listing doesn't
// tell: YouTube playlists and playlist-like listings keep playlist order,
// everything else (channel and user pages) lists newest first.
func guessNewestFirst(src *store.Source, l *ytdlp.Listing) bool {
	if src.Tab != "" {
		return true
	}
	if isYouTube(src, l) {
		return false
	}
	return !playlistLike(l.ExtractorKey)
}

// listingOrder returns the order a listing shows by itself: a YouTube tab,
// or the dates of its first and last dated entries. ok is false if it
// can't tell.
func listingOrder(src *store.Source, l *ytdlp.Listing) (newestFirst, ok bool) {
	if src.Tab != "" {
		return true, true
	}
	var first, last time.Time
	for _, le := range l.Entries {
		if _, t := listingDate(le); !t.IsZero() {
			if first.IsZero() {
				first = t
			}
			last = t
		}
	}
	if first.Equal(last) {
		return false, false
	}
	return first.After(last), true
}

// orderFromNew tells the order from where new entries appeared in a
// complete listing of n entries: a block at the start means newest first,
// a block at the end oldest first.
func orderFromNew(pos []int, n int) (newestFirst, ok bool) {
	k := len(pos)
	if k == 0 || k >= n {
		return false, false
	}
	top, bottom := true, true
	for _, p := range pos {
		if p >= k {
			top = false
		}
		if p < n-k {
			bottom = false
		}
	}
	switch {
	case top && !bottom:
		return true, true
	case bottom && !top:
		return false, true
	}
	return false, false
}

// undatedSort is the sort date of an entry without a date: the time it was
// first seen, ordered within one listing by position.
func undatedSort(seen time.Time, pos, n int, newestFirst bool) time.Time {
	if newestFirst {
		return seen.Add(-time.Duration(pos) * time.Millisecond)
	}
	return seen.Add(-time.Duration(n-pos) * time.Millisecond)
}

// Windowed reports whether a source is known to list newest first, so
// checks may list only its newest entries.
func Windowed(src *store.Source) bool {
	return src.Tab != "" || src.OrderKnown && src.NewestFirst
}
