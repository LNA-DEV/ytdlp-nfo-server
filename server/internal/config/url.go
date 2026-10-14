package config

import (
	"net/url"
	"sort"
	"strings"
)

var siteAliases = map[string]string{
	"youtu.be":              "youtube.com",
	"youtube-nocookie.com":  "youtube.com",
	"music.youtube.com":     "youtube.com",
	"m.youtube.com":         "youtube.com",
	"vm.tiktok.com":         "tiktok.com",
	"x.com":                 "twitter.com",
	"mobile.twitter.com":    "twitter.com",
	"old.reddit.com":        "reddit.com",
	"clips.twitch.tv":       "twitch.tv",
	"player.vimeo.com":      "vimeo.com",
	"dai.ly":                "dailymotion.com",
	"open.spotify.com":      "spotify.com",
	"soundcloud.app.goo.gl": "soundcloud.com",
}

var twoLevelTLDs = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "com.au": true, "net.au": true, "co.jp": true,
	"co.nz": true, "com.br": true, "co.in": true, "co.kr": true, "com.tr": true, "com.mx": true,
}

// SiteKey groups hosts for per-site limits and cooldowns: the registrable
// domain, with a few well-known aliases folded together.
func SiteKey(host string) string {
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	if a, ok := siteAliases[host]; ok {
		return a
	}
	labels := strings.Split(host, ".")
	n := 2
	if len(labels) >= 3 && twoLevelTLDs[strings.Join(labels[len(labels)-2:], ".")] {
		n = 3
	}
	if len(labels) <= n {
		if a, ok := siteAliases[host]; ok {
			return a
		}
		return host
	}
	key := strings.Join(labels[len(labels)-n:], ".")
	if a, ok := siteAliases[key]; ok {
		return a
	}
	return key
}

var trackingParams = map[string]bool{
	"si": true, "feature": true, "fbclid": true, "gclid": true, "igshid": true, "ref": true,
	"ref_src": true, "pp": true, "ab_channel": true, "t": true, "start": true,
}

// NormalizeURL returns a canonical form used for duplicate detection
// (F20): youtu.be/x and youtube.com/watch?v=x compare equal, tracking
// parameters and fragments are dropped, http and https are the same.
func NormalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "m.")
	q := u.Query()
	p := strings.TrimRight(u.EscapedPath(), "/")

	switch host {
	case "youtu.be":
		if id := strings.Trim(u.Path, "/"); id != "" {
			host, p = "youtube.com", "/watch"
			q.Set("v", id)
		}
	case "youtube.com", "music.youtube.com", "youtube-nocookie.com":
		host = "youtube.com"
		for _, pre := range []string{"/shorts/", "/live/", "/embed/", "/v/"} {
			if id, ok := strings.CutPrefix(u.Path, pre); ok && id != "" && !strings.Contains(id, "/") {
				p = "/watch"
				q.Set("v", id)
			}
		}
		if p == "/watch" {
			keep := url.Values{}
			if v := q.Get("v"); v != "" {
				keep.Set("v", v)
			}
			if l := q.Get("list"); l != "" {
				keep.Set("list", l)
			}
			q = keep
		}
	}

	for k := range q {
		if trackingParams[k] || strings.HasPrefix(k, "utm_") {
			q.Del(k)
		}
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("https://")
	b.WriteString(host)
	if port := u.Port(); port != "" && port != "80" && port != "443" {
		b.WriteString(":" + port)
	}
	b.WriteString(p)
	for i, k := range keys {
		if i == 0 {
			b.WriteByte('?')
		} else {
			b.WriteByte('&')
		}
		vals := q[k]
		sort.Strings(vals)
		for j, v := range vals {
			if j > 0 {
				b.WriteByte('&')
			}
			b.WriteString(url.QueryEscape(k) + "=" + url.QueryEscape(v))
		}
	}
	return b.String()
}
