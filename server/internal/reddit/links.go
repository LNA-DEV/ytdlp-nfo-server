package reddit

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
)

// Links to other sites (Reddit concept §7.6): a link post whose target is
// on the allowlist becomes an ordinary download of its target.

// LinkTarget is where a link post's media lives.
type LinkTarget struct {
	// URL is what the item downloads. For a known URL form it is built
	// from the validated id, never taken from the post.
	URL string
	// Key is the archive key yt-dlp writes for the target ("youtube <id>"),
	// or "url <normalized URL>" for a host without a known form.
	Key  string
	Site string // the target host, e.g. "www.youtube.com"
	// Image marks an Imgur still image: yt-dlp's Imgur extractor rejects
	// those, so it goes through the image path of the Reddit pipeline.
	Image bool
	ID    string // the id of a known form
}

// Links resolves link posts against the allowlist (the setting
// reddit.externalHosts). A "*.example.com" entry covers example.com and
// its subdomains.
type Links struct {
	Hosts []string
	// Public, if set, checks that a host without a known URL form resolves
	// to public addresses only (the check of §10).
	Public func(ctx context.Context, host string) bool
}

// Allowed reports whether a host is on the allowlist.
func (l Links) Allowed(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, p := range l.Hosts {
		p = strings.ToLower(strings.TrimSpace(p))
		if d, ok := strings.CutPrefix(p, "*."); ok {
			if host == d || strings.HasSuffix(host, "."+d) {
				return true
			}
		} else if host == p {
			return true
		}
	}
	return false
}

// Resolve returns the target of a link; ok is false for a link that is not
// followed: a host off the allowlist, an Imgur album, a link to Reddit, a
// plain-HTTP link without a known form, or a private address.
func (l Links) Resolve(ctx context.Context, raw string) (LinkTarget, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return LinkTarget{}, false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || IsRedditHost(host) || strings.HasSuffix(host, ".redd.it") || !l.Allowed(host) {
		return LinkTarget{}, false
	}
	if t, known, ok := knownForm(u, host); known {
		return t, ok
	}
	if u.Scheme != "https" || (u.Port() != "" && u.Port() != "443") || config.ValidateURL(u.String()) != nil {
		return LinkTarget{}, false
	}
	if l.Public != nil && !l.Public(ctx, host) {
		return LinkTarget{}, false
	}
	return LinkTarget{URL: u.String(), Key: "url " + config.NormalizeURL(u.String()), Site: host}, true
}

var (
	youtubeIDRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	redgifsIDRe    = regexp.MustCompile(`^[A-Za-z0-9]{3,64}$`)
	streamableIDRe = regexp.MustCompile(`^[A-Za-z0-9]{3,16}$`)
	imgurIDRe      = regexp.MustCompile(`^[A-Za-z0-9]{5,10}$`)
)

// knownForm recognizes the URL forms of §7.6. known reports whether the
// host is one of the known sites; ok whether the link is followed (an
// Imgur album on a known host is not).
func knownForm(u *url.URL, host string) (t LinkTarget, known, ok bool) {
	parts := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	first := func() string {
		if len(parts) > 0 {
			return parts[0]
		}
		return ""
	}
	switch {
	case host == "youtu.be" || host == "youtube.com" || strings.HasSuffix(host, ".youtube.com"):
		id := ""
		switch {
		case host == "youtu.be":
			id = first()
		case first() == "watch":
			id = u.Query().Get("v")
		case len(parts) == 2 && (parts[0] == "shorts" || parts[0] == "live" || parts[0] == "embed"):
			id = parts[1]
		}
		if !youtubeIDRe.MatchString(id) {
			return t, true, false
		}
		return LinkTarget{URL: "https://www.youtube.com/watch?v=" + id, Key: "youtube " + id, Site: "www.youtube.com", ID: id}, true, true
	case host == "redgifs.com" || strings.HasSuffix(host, ".redgifs.com"):
		if len(parts) != 2 || (parts[0] != "watch" && parts[0] != "ifr") || !redgifsIDRe.MatchString(parts[1]) {
			return t, true, false
		}
		id := strings.ToLower(parts[1]) // yt-dlp lowercases Redgifs ids
		return LinkTarget{URL: "https://www.redgifs.com/watch/" + id, Key: "redgifs " + id, Site: "www.redgifs.com", ID: id}, true, true
	case host == "streamable.com" || host == "www.streamable.com":
		id := first()
		if len(parts) == 2 && (parts[0] == "e" || parts[0] == "s") {
			id = parts[1]
		} else if len(parts) != 1 {
			id = ""
		}
		if !streamableIDRe.MatchString(id) {
			return t, true, false
		}
		return LinkTarget{URL: "https://streamable.com/" + id, Key: "streamable " + id, Site: "streamable.com", ID: id}, true, true
	case host == "imgur.com" || strings.HasSuffix(host, ".imgur.com"):
		if len(parts) != 1 {
			return t, true, false // albums (/a/…), galleries (/gallery/…), tags
		}
		id, ext, _ := strings.Cut(parts[0], ".")
		if !imgurIDRe.MatchString(id) {
			return t, true, false
		}
		switch strings.ToLower(ext) {
		case "jpg", "jpeg", "png", "webp":
			if host != "i.imgur.com" {
				return t, true, false
			}
			e := strings.ToLower(ext)
			return LinkTarget{URL: "https://i.imgur.com/" + id + "." + e, Key: "imgur " + id, Site: "i.imgur.com", Image: true, ID: id}, true, true
		case "", "gifv", "mp4", "gif":
			return LinkTarget{URL: "https://imgur.com/" + id, Key: "imgur " + id, Site: "imgur.com", ID: id}, true, true
		}
		return t, true, false
	}
	return t, false, false
}
