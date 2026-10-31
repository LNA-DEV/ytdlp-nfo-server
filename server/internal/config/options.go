package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Options are the effective options of a job, stored on the job.
type Options struct {
	Container     string  `json:"container"`
	AllAudio      bool    `json:"allAudio"`
	Subtitles     bool    `json:"subtitles"`
	Preset        string  `json:"preset"`
	Impersonate   string  `json:"impersonate"`
	CookieProfile string  `json:"cookieProfile,omitempty"`
	YtdlpVersion  string  `json:"ytdlpVersion,omitempty"`
	Force         bool    `json:"force,omitempty"`
	Library       string  `json:"library"`
	LimitRate     string  `json:"limitRate,omitempty"`
	SleepRequests float64 `json:"sleepRequests,omitempty"`
	SleepInterval float64 `json:"sleepInterval,omitempty"`
	SiteRuleID    int64   `json:"siteRuleId,omitempty"`
	// RedditProviders is a subscription's Reddit provider chain override
	// ("native,arcticshift"); empty uses the global chain (Reddit concept §9).
	RedditProviders string `json:"redditProviders,omitempty"`
}

// OptionsRequest carries explicitly requested options. Nil means "not set",
// so site rules and global defaults apply (§7.2 precedence).
type OptionsRequest struct {
	Container     *string `json:"container,omitempty"`
	Format        *string `json:"format,omitempty"` // v1 name for container
	AllAudio      *bool   `json:"allAudio,omitempty"`
	Subtitles     *bool   `json:"subtitles,omitempty"`
	Preset        *string `json:"preset,omitempty"`
	Impersonate   *string `json:"impersonate,omitempty"`
	CookieProfile *string `json:"cookieProfile,omitempty"`
	YtdlpVersion  *string `json:"ytdlpVersion,omitempty"`
	Force         *bool   `json:"force,omitempty"`
	Library       *string `json:"library,omitempty"`
}

// Merge returns r with fields from o overriding (used for retry overrides).
func (r OptionsRequest) Merge(o OptionsRequest) OptionsRequest {
	if o.Container != nil {
		r.Container = o.Container
	}
	if o.Format != nil {
		r.Format = o.Format
	}
	if o.AllAudio != nil {
		r.AllAudio = o.AllAudio
	}
	if o.Subtitles != nil {
		r.Subtitles = o.Subtitles
	}
	if o.Preset != nil {
		r.Preset = o.Preset
	}
	if o.Impersonate != nil {
		r.Impersonate = o.Impersonate
	}
	if o.CookieProfile != nil {
		r.CookieProfile = o.CookieProfile
	}
	if o.YtdlpVersion != nil {
		r.YtdlpVersion = o.YtdlpVersion
	}
	if o.Force != nil {
		r.Force = o.Force
	}
	if o.Library != nil {
		r.Library = o.Library
	}
	return r
}

// Validate checks requested values that don't need the settings.
func (r OptionsRequest) Validate() error {
	if c := r.container(); c != nil && !ValidContainer(*c) {
		return fmt.Errorf("unknown container %q", *c)
	}
	if r.Impersonate != nil {
		if err := ValidateImpersonate(*r.Impersonate); err != nil {
			return err
		}
	}
	if r.YtdlpVersion != nil {
		if err := ValidateVersionSpec(*r.YtdlpVersion); err != nil {
			return err
		}
	}
	return nil
}

func (r OptionsRequest) container() *string {
	if r.Container != nil {
		return r.Container
	}
	return r.Format
}

// ValidateVersionSpec accepts "", "<channel>:latest" and "<channel>/<tag>".
func ValidateVersionSpec(s string) error {
	if s == "" {
		return nil
	}
	if ch, ok := strings.CutSuffix(s, ":latest"); ok {
		if !ValidChannel(ch) {
			return fmt.Errorf("unknown channel in %q", s)
		}
		return nil
	}
	ch, tag, ok := strings.Cut(s, "/")
	if !ok || !ValidChannel(ch) && ch != "bundled" && ch != "system" || tag == "" {
		return fmt.Errorf("invalid yt-dlp version %q", s)
	}
	for _, c := range tag {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' || c == '-' || c == '_') {
			return fmt.Errorf("invalid yt-dlp version %q", s)
		}
	}
	return nil
}

// Host returns the lowercased host of a URL.
func Host(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Resolve computes effective options: explicit job option > site rule >
// global default.
func Resolve(req OptionsRequest, s *Settings, rules []SiteRule, rawURL string) (Options, error) {
	d := s.Defaults
	o := Options{
		Container:   d.Container,
		AllAudio:    d.AllAudio,
		Subtitles:   d.Subtitles,
		Preset:      d.Preset,
		Impersonate: d.Impersonate,
		Library:     d.Library,
	}
	if r := MatchRule(rules, Host(rawURL)); r != nil {
		o.SiteRuleID = r.ID
		if r.Impersonate != "" {
			o.Impersonate = r.Impersonate
		}
		if r.CookieProfile != "" {
			o.CookieProfile = r.CookieProfile
		}
		if r.Preset != "" {
			o.Preset = r.Preset
		}
		if r.Library != "" {
			o.Library = r.Library
		}
		o.LimitRate = r.LimitRate
		o.SleepRequests = r.SleepRequests
		o.SleepInterval = r.SleepInterval
	}
	if c := req.container(); c != nil {
		o.Container = *c
	}
	if req.AllAudio != nil {
		o.AllAudio = *req.AllAudio
	}
	if req.Subtitles != nil {
		o.Subtitles = *req.Subtitles
	}
	if req.Preset != nil && *req.Preset != "" {
		o.Preset = *req.Preset
	}
	if req.Impersonate != nil {
		o.Impersonate = *req.Impersonate
	}
	if req.CookieProfile != nil {
		o.CookieProfile = *req.CookieProfile
	}
	if req.YtdlpVersion != nil {
		o.YtdlpVersion = *req.YtdlpVersion
	}
	if req.Force != nil {
		o.Force = *req.Force
	}
	if req.Library != nil && *req.Library != "" {
		o.Library = *req.Library
	}
	if err := req.Validate(); err != nil {
		return o, err
	}
	if !ValidContainer(o.Container) {
		return o, fmt.Errorf("unknown container %q", o.Container)
	}
	if _, ok := s.Presets[o.Preset]; !ok {
		return o, fmt.Errorf("unknown preset %q", o.Preset)
	}
	if !s.HasLibrary(o.Library) {
		return o, fmt.Errorf("unknown library %q", o.Library)
	}
	return o, nil
}

// HasLibrary reports whether a library name is configured.
func (s *Settings) HasLibrary(name string) bool {
	if name == DefaultLibrary {
		return true
	}
	for _, l := range s.Libraries {
		if l.Name == name {
			return true
		}
	}
	return false
}

// LibraryPath returns the root path of a library.
func (s *Settings) LibraryPath(e *Env, name string) (string, bool) {
	if name == "" || name == DefaultLibrary {
		return e.LibraryRoot(), true
	}
	for _, l := range s.Libraries {
		if l.Name == name {
			return l.Path, true
		}
	}
	return "", false
}
