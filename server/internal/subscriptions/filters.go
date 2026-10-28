package subscriptions

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// FilterPrefix starts the ignore reason of an entry ignored by a filter.
const FilterPrefix = "filter: "

// Ignore reasons set automatically (an empty reason is the user's).
const (
	ReasonNotAVideo   = "not a video"
	ReasonNeedsCookie = "needs cookies"
	ReasonNoMedia     = "no media"
	ReasonLinkPrefix  = "link to "
)

// linkHost returns the host of a link post's target, for its ignore reason.
func linkHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	}
	return "another site"
}

// maxPattern limits title patterns (§4.1).
const maxPattern = 200

// Filter is a compiled store.Filters.
type Filter struct {
	f        store.Filters
	include  *regexp.Regexp
	exclude  *regexp.Regexp
	isActive bool
}

// CompileFilters validates and compiles filters. Title patterns are RE2,
// case-insensitive and at most 200 characters.
func CompileFilters(f store.Filters) (*Filter, error) {
	c := &Filter{f: f}
	var err error
	compile := func(name, p string) (*regexp.Regexp, error) {
		if p == "" {
			return nil, nil
		}
		if len(p) > maxPattern {
			return nil, fmt.Errorf("%s pattern is longer than %d characters", name, maxPattern)
		}
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			return nil, fmt.Errorf("%s pattern: %v", name, err)
		}
		return re, nil
	}
	if c.include, err = compile("title include", f.TitleInclude); err != nil {
		return nil, err
	}
	if c.exclude, err = compile("title exclude", f.TitleExclude); err != nil {
		return nil, err
	}
	if f.MinDuration < 0 || f.MaxDuration < 0 || f.MaxDuration > 0 && f.MaxDuration < f.MinDuration {
		return nil, fmt.Errorf("invalid duration range")
	}
	if f.PublishedAfter != "" {
		if _, err := time.Parse("20060102", f.PublishedAfter); err != nil {
			return nil, fmt.Errorf("publishedAfter must be YYYYMMDD")
		}
	}
	for _, k := range f.MediaKinds {
		switch k {
		case "image", "gallery", "video", "gif", "linked":
		default:
			return nil, fmt.Errorf("unknown media kind %q", k)
		}
	}
	switch f.NSFW {
	case "", store.NSFWInclude, store.NSFWExclude, store.NSFWOnly:
	default:
		return nil, fmt.Errorf("nsfw must be include, exclude or only")
	}
	c.isActive = !f.Empty()
	return c, nil
}

// Active reports whether any filter is set.
func (c *Filter) Active() bool { return c != nil && c.isActive }

// Reason returns why an entry is filtered out ("" if it passes). Filters
// use listing data only: an unknown duration or upload date passes.
func (c *Filter) Reason(e *store.Entry) string {
	if !c.Active() {
		return ""
	}
	f := c.f
	switch {
	case f.MinDuration > 0 && e.Duration > 0 && e.Duration < f.MinDuration:
		return FilterPrefix + "shorter than " + clock(f.MinDuration)
	case f.MaxDuration > 0 && e.Duration > 0 && e.Duration > f.MaxDuration:
		return FilterPrefix + "longer than " + clock(f.MaxDuration)
	case c.include != nil && !c.include.MatchString(e.Title):
		return FilterPrefix + "title does not match " + quote(f.TitleInclude)
	case c.exclude != nil && c.exclude.MatchString(e.Title):
		return FilterPrefix + "title matches " + quote(f.TitleExclude)
	case f.PublishedAfter != "" && e.UploadDate != "" && e.UploadDate < f.PublishedAfter:
		return FilterPrefix + "published before " + f.PublishedAfter[:4] + "-" + f.PublishedAfter[4:6] + "-" + f.PublishedAfter[6:]
	case len(f.MediaKinds) > 0 && e.MediaKind != "" && !slices.Contains(f.MediaKinds, filterKind(e.MediaKind)):
		return FilterPrefix + filterKind(e.MediaKind)
	case f.NSFW == store.NSFWExclude && e.NSFW:
		return FilterPrefix + "NSFW"
	case f.NSFW == store.NSFWOnly && !e.NSFW && e.MediaKind != "":
		return FilterPrefix + "not NSFW"
	}
	return ""
}

// filterKind maps an entry's media kind to its filter name: a link post's
// media is "linked".
func filterKind(k string) string {
	if k == "link" {
		return "linked"
	}
	return k
}

func quote(s string) string {
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return `"` + s + `"`
}

// clock formats seconds as m:ss or h:mm:ss.
func clock(sec float64) string {
	d := int(sec)
	if d >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", d/3600, d%3600/60, d%60)
	}
	return fmt.Sprintf("%d:%02d", d/60, d%60)
}

// IsFilterReason reports whether an ignore reason was set by a filter.
func IsFilterReason(r string) bool { return strings.HasPrefix(r, FilterPrefix) }
