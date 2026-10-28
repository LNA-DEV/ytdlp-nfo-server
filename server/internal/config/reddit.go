package config

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Reddit providers (Reddit concept §5, §6).
const (
	RedditNative      = "native"
	RedditArcticShift = "arcticshift"
)

// RedditSettings are the global Reddit values (Reddit concept §9).
type RedditSettings struct {
	// Providers is the global chain, in order (R3). Arctic Shift is a
	// third-party service that sees what the server asks for (R5).
	Providers []string `json:"providers"`
	// Proxy routes Reddit and Arctic Shift requests and media downloads
	// (http://, https:// or socks5://).
	Proxy            string `json:"proxy"`
	FirstListing     int    `json:"firstListing"`     // depth of a first listing and a full rescan
	JobListLimit     int    `json:"jobListLimit"`     // posts of a pasted subreddit URL
	PageDelaySeconds int    `json:"pageDelaySeconds"` // between two native pages (±jitter)
	DefaultLibrary   string `json:"defaultLibrary"`   // preselected for new Reddit subscriptions
	GapFill          bool   `json:"gapFill"`          // fill gaps from Arctic Shift (§7.7)
	BackfillMax      int    `json:"backfillMax"`      // posts per gap fill or backfill run
	// Duplicates are the levels 2 and 3 of the duplicate check (§7.5).
	Duplicates RedditDuplicates `json:"duplicates"`
	// ExternalHosts are the sites whose link posts are downloaded (§7.6):
	// hosts or "*.domain" (the domain and its subdomains).
	ExternalHosts []string `json:"externalHosts"`
}

// DefaultRedditExternalHosts returns the sites followed by default.
func DefaultRedditExternalHosts() []string {
	return []string{"*.youtube.com", "youtu.be", "*.redgifs.com", "streamable.com", "*.imgur.com"}
}

var hostPatternRe = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// ValidHostPattern reports whether p is an allowlist entry: a host name
// with a dot, or "*." and a domain.
func ValidHostPattern(p string) bool { return hostPatternRe.MatchString(p) }

// Modes of level 3 (similar images).
const (
	SimilarOff  = "off"
	SimilarWarn = "warn"
	SimilarSkip = "skip"
)

// RedditDuplicates configure the duplicate check beyond media keys.
type RedditDuplicates struct {
	ContentHash     bool   `json:"contentHash"`     // level 2: skip files identical to library files
	Similar         string `json:"similar"`         // level 3: off, warn or skip
	SimilarDistance int    `json:"similarDistance"` // bits two dHashes may differ in
}

// DefaultRedditSettings returns the documented defaults.
func DefaultRedditSettings() RedditSettings {
	return RedditSettings{Providers: []string{RedditNative, RedditArcticShift}, FirstListing: 100, JobListLimit: 25,
		PageDelaySeconds: 2, GapFill: true, BackfillMax: 5000,
		Duplicates:    RedditDuplicates{ContentHash: true, Similar: SimilarWarn, SimilarDistance: 6},
		ExternalHosts: DefaultRedditExternalHosts()}
}

// normalize fills settings stored before Reddit support existed.
func (r *RedditSettings) normalize() {
	d := DefaultRedditSettings()
	if r.Providers == nil {
		*r = d
		return
	}
	if r.Duplicates.Similar == "" { // stored before level 3 existed
		r.Duplicates = d.Duplicates
	}
	if r.ExternalHosts == nil { // stored before links were followed
		r.ExternalHosts = d.ExternalHosts
	}
	for i, h := range r.ExternalHosts {
		r.ExternalHosts[i] = strings.ToLower(strings.TrimSpace(h))
	}
	for _, f := range []struct{ v, d *int }{
		{&r.FirstListing, &d.FirstListing}, {&r.JobListLimit, &d.JobListLimit},
		{&r.PageDelaySeconds, &d.PageDelaySeconds}, {&r.BackfillMax, &d.BackfillMax},
	} {
		if *f.v <= 0 {
			*f.v = *f.d
		}
	}
}

// ValidRedditProvider reports whether p is a provider name.
func ValidRedditProvider(p string) bool { return p == RedditNative || p == RedditArcticShift }

// ValidateRedditChain checks an ordered provider list.
func ValidateRedditChain(ps []string) error {
	for i, p := range ps {
		if !ValidRedditProvider(p) {
			return fmt.Errorf("unknown Reddit provider %q", p)
		}
		if slices.Contains(ps[:i], p) {
			return fmt.Errorf("Reddit provider %q listed twice", p)
		}
	}
	return nil
}

// Validate checks the ranges of §9.
func (r RedditSettings) Validate(libraries map[string]bool) error {
	if err := ValidateRedditChain(r.Providers); err != nil {
		return err
	}
	if r.Proxy != "" {
		u, err := url.Parse(r.Proxy)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return fmt.Errorf("reddit.proxy must be an http://, https:// or socks5:// URL")
		}
	}
	for _, x := range []struct {
		name      string
		v, lo, hi int
	}{
		{"firstListing", r.FirstListing, 25, 1000},
		{"jobListLimit", r.JobListLimit, 1, 1000},
		{"pageDelaySeconds", r.PageDelaySeconds, 1, 60},
		{"backfillMax", r.BackfillMax, 100, 50000},
	} {
		if x.v < x.lo || x.v > x.hi {
			return fmt.Errorf("reddit.%s must be between %d and %d", x.name, x.lo, x.hi)
		}
	}
	switch r.Duplicates.Similar {
	case SimilarOff, SimilarWarn, SimilarSkip:
	default:
		return fmt.Errorf("reddit.duplicates.similar must be off, warn or skip")
	}
	if d := r.Duplicates.SimilarDistance; d < 0 || d > 16 {
		return fmt.Errorf("reddit.duplicates.similarDistance must be between 0 and 16")
	}
	for _, h := range r.ExternalHosts {
		if !ValidHostPattern(h) {
			return fmt.Errorf("reddit.externalHosts: %q is not a host or *.domain", h)
		}
	}
	if r.DefaultLibrary != "" && !libraries[r.DefaultLibrary] {
		return fmt.Errorf("reddit.defaultLibrary %q does not exist", r.DefaultLibrary)
	}
	return nil
}
