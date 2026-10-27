package config

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Channels of official yt-dlp builds.
const (
	ChannelStable  = "stable"
	ChannelNightly = "nightly"
	ChannelMaster  = "master"
)

// Channels lists all channels in UI order.
var Channels = []string{ChannelStable, ChannelNightly, ChannelMaster}

// ValidChannel reports whether c is a known channel.
func ValidChannel(c string) bool { return slices.Contains(Channels, c) }

// Auto-update policies per channel.
const (
	UpdateOff    = "off"
	UpdateNotify = "notify"
	UpdateAuto   = "auto"
)

// Naming presets.
const (
	NamingDefault = "default"
	NamingLegacy  = "legacy"
	NamingCustom  = "custom"

	TemplateDefault = "{playlist_title?}/{title} [{id}]/{title} [{id}].{ext}"
	TemplateLegacy  = "{playlist_title?}/{title}/{title}.{ext}"
)

// DefaultLibrary is the name of the library backed by OUTPUT_DIR.
const DefaultLibrary = "default"

// Library is a named output root.
type Library struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Defaults are the global defaults for new jobs.
type Defaults struct {
	Preset      string `json:"preset"`
	Container   string `json:"container"`
	AllAudio    bool   `json:"allAudio"`
	Subtitles   bool   `json:"subtitles"`
	Impersonate string `json:"impersonate"`
	Library     string `json:"library"`
}

// Naming selects the library layout.
type Naming struct {
	Preset   string `json:"preset"`
	Template string `json:"template"`
}

// EffectiveTemplate returns the template for the selected preset.
func (n Naming) EffectiveTemplate() string {
	switch n.Preset {
	case NamingLegacy:
		return TemplateLegacy
	case NamingCustom:
		if strings.TrimSpace(n.Template) != "" {
			return n.Template
		}
	}
	return TemplateDefault
}

// YtdlpSettings configures the version manager.
type YtdlpSettings struct {
	Channel              string            `json:"channel"`
	Active               string            `json:"active"` // version id "<channel>/<tag>"
	AutoUpdate           map[string]string `json:"autoUpdate"`
	NightlyRetryOnBroken bool              `json:"nightlyRetryOnBroken"`
	SmokeTestURL         string            `json:"smokeTestURL"`
}

// Extras configures optional output features. All off by default (§16.4).
type Extras struct {
	ConvertSubsSRT   bool `json:"convertSubsSRT"`
	EmbedSubs        bool `json:"embedSubs"`
	EmbedChapters    bool `json:"embedChapters"`
	SponsorBlockMark bool `json:"sponsorBlockMark"`
}

// Notifications are sent when a job finishes.
type Notifications struct {
	WebhookURL        string `json:"webhookURL"`
	NtfyURL           string `json:"ntfyURL"`
	GotifyURL         string `json:"gotifyURL"`
	GotifyToken       string `json:"gotifyToken"`
	DiscordWebhookURL string `json:"discordWebhookURL"`
	OnCompleted       bool   `json:"onCompleted"`
	OnFailed          bool   `json:"onFailed"`
}

// PathMap rewrites a server path prefix to the path Jellyfin sees.
type PathMap struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Jellyfin configures the library refresh after finalize.
type Jellyfin struct {
	URL     string    `json:"url"`
	APIKey  string    `json:"apiKey"`
	PathMap []PathMap `json:"pathMap"`
}

// AgeRule maps an extractor name substring to an NFO rating.
type AgeRule struct {
	Match  string `json:"match"`
	Rating string `json:"rating"`
}

// SubscriptionSettings are the global subscription values (subscriptions
// concept §10).
type SubscriptionSettings struct {
	Paused           bool `json:"paused"`           // pause all checks
	IntervalMinutes  int  `json:"intervalMinutes"`  // default check interval
	CheckConcurrency int  `json:"checkConcurrency"` // parallel checks (at most one per site)
	SiteDelaySeconds int  `json:"siteDelaySeconds"` // pause between checks of the same site
	Window           int  `json:"window"`           // window size for newest-first sources
	FullRescanDays   int  `json:"fullRescanDays"`   // full listing interval per source
	AutoQueueCap     int  `json:"autoQueueCap"`     // safety cap per check (S8)
	BackgroundSlots  int  `json:"backgroundSlots"`  // slots for auto and backlog jobs together (S7)
	UpcomingDays     int  `json:"upcomingDays"`     // how long "not available yet" entries are queued again
	PruneGoneDays    int  `json:"pruneGoneDays"`    // delete gone, never downloaded entries (0 = never)
}

// DefaultSubscriptionSettings returns the documented defaults.
func DefaultSubscriptionSettings() SubscriptionSettings {
	return SubscriptionSettings{IntervalMinutes: 360, CheckConcurrency: 2, SiteDelaySeconds: 10, Window: 50,
		FullRescanDays: 7, AutoQueueCap: 25, BackgroundSlots: 1, UpcomingDays: 7}
}

// Limits of subscription intervals.
const (
	MinIntervalMinutes = 15
	MaxIntervalMinutes = 10080
)

// Validate checks the ranges of §10.
func (s SubscriptionSettings) Validate() error {
	for _, r := range []struct {
		name      string
		v, lo, hi int
	}{
		{"intervalMinutes", s.IntervalMinutes, MinIntervalMinutes, MaxIntervalMinutes},
		{"checkConcurrency", s.CheckConcurrency, 1, 10},
		{"siteDelaySeconds", s.SiteDelaySeconds, 0, 300},
		{"window", s.Window, 10, 500},
		{"fullRescanDays", s.FullRescanDays, 1, 90},
		{"autoQueueCap", s.AutoQueueCap, 1, 1000},
		{"backgroundSlots", s.BackgroundSlots, 1, 50},
		{"upcomingDays", s.UpcomingDays, 1, 30},
		{"pruneGoneDays", s.PruneGoneDays, 0, 3650},
	} {
		if r.v < r.lo || r.v > r.hi {
			return fmt.Errorf("subscriptions.%s must be between %d and %d", r.name, r.lo, r.hi)
		}
	}
	return nil
}

// Settings are the runtime settings persisted in the store.
type Settings struct {
	MaxConcurrent            int                  `json:"maxConcurrent"`
	MaxRetries               int                  `json:"maxRetries"`
	Defaults                 Defaults             `json:"defaults"`
	Presets                  map[string]string    `json:"presets"`
	Naming                   Naming               `json:"naming"`
	Ytdlp                    YtdlpSettings        `json:"ytdlp"`
	ImpersonationEscalation  bool                 `json:"impersonationEscalation"`
	RateLimitCooldownMinutes int                  `json:"rateLimitCooldownMinutes"`
	Extras                   Extras               `json:"extras"`
	AutoPruneDays            int                  `json:"autoPruneDays"`
	Libraries                []Library            `json:"libraries"`
	Notifications            Notifications        `json:"notifications"`
	Jellyfin                 Jellyfin             `json:"jellyfin"`
	AgeRules                 []AgeRule            `json:"ageRules"`
	MinFreeSpaceMB           int                  `json:"minFreeSpaceMB"`
	Subscriptions            SubscriptionSettings `json:"subscriptions"`
	PasswordHash             string               `json:"passwordHash,omitempty"`

	// Locked lists settings fixed by environment variables. Not persisted.
	Locked []string `json:"locked,omitempty"`
}

// DefaultPresets are the quality presets (yt-dlp -S sort strings).
func DefaultPresets() map[string]string {
	return map[string]string{
		"best":       "",
		"compatible": "res:1080,vcodec:h264,acodec:aac",
		"max1080":    "res:1080",
		"max720":     "res:720",
	}
}

// DefaultSettings returns defaults. migrated selects the v1-compatible
// defaults (legacy naming, stable channel) for upgraded installs (§16.2/3).
func DefaultSettings(migrated bool) Settings {
	s := Settings{
		MaxConcurrent: 3,
		MaxRetries:    3,
		Defaults: Defaults{
			Preset:    "best",
			Container: "mkv",
			AllAudio:  true,
			Subtitles: true,
			Library:   DefaultLibrary,
		},
		Presets: DefaultPresets(),
		Naming:  Naming{Preset: NamingDefault},
		Ytdlp: YtdlpSettings{
			Channel: ChannelNightly,
			AutoUpdate: map[string]string{
				ChannelStable:  UpdateAuto,
				ChannelNightly: UpdateAuto,
				ChannelMaster:  UpdateNotify,
			},
			NightlyRetryOnBroken: true,
		},
		ImpersonationEscalation:  true,
		RateLimitCooldownMinutes: 30,
		Notifications:            Notifications{OnCompleted: true, OnFailed: true},
		AgeRules: []AgeRule{
			{Match: "porn", Rating: "XXX"},
			{Match: "fetish", Rating: "XXX"},
		},
		MinFreeSpaceMB: 1024,
		Subscriptions:  DefaultSubscriptionSettings(),
	}
	if migrated {
		s.Naming.Preset = NamingLegacy
		s.Ytdlp.Channel = ChannelStable
	}
	return s
}

// ApplyEnv overrides settings that were set explicitly in the environment
// and records them as locked.
func (s *Settings) ApplyEnv(e *Env) {
	s.Locked = nil
	if e.MaxConcurrent != nil {
		s.MaxConcurrent = *e.MaxConcurrent
		s.Locked = append(s.Locked, "maxConcurrent")
	}
	if e.MaxRetries != nil {
		s.MaxRetries = *e.MaxRetries
		s.Locked = append(s.Locked, "maxRetries")
	}
	if e.YtdlpChannel != "" {
		s.Ytdlp.Channel = e.YtdlpChannel
		s.Locked = append(s.Locked, "ytdlp.channel")
	}
	if e.JellyfinURL != "" {
		s.Jellyfin.URL = e.JellyfinURL
		s.Locked = append(s.Locked, "jellyfin.url")
	}
	if e.JellyfinAPIKey != "" {
		s.Jellyfin.APIKey = e.JellyfinAPIKey
		s.Locked = append(s.Locked, "jellyfin.apiKey")
	}
	if e.Password != "" {
		s.Locked = append(s.Locked, "password")
	}
	// Libraries from the environment are always present.
	for _, l := range e.Libraries {
		if i := slices.IndexFunc(s.Libraries, func(x Library) bool { return x.Name == l.Name }); i >= 0 {
			s.Libraries[i] = l
		} else {
			s.Libraries = append(s.Libraries, l)
		}
	}
}

// IsLocked reports whether a setting is fixed by the environment.
func (s *Settings) IsLocked(name string) bool { return slices.Contains(s.Locked, name) }

// Normalize fills missing fields (e.g. after loading an older JSON blob).
func (s *Settings) Normalize() {
	d := DefaultSettings(false)
	if s.MaxConcurrent <= 0 {
		s.MaxConcurrent = d.MaxConcurrent
	}
	if s.MaxRetries < 0 {
		s.MaxRetries = d.MaxRetries
	}
	if s.Presets == nil {
		s.Presets = DefaultPresets()
	}
	if s.Defaults.Container == "" {
		s.Defaults.Container = "mkv"
	}
	if s.Defaults.Preset == "" {
		s.Defaults.Preset = "best"
	}
	if s.Defaults.Library == "" {
		s.Defaults.Library = DefaultLibrary
	}
	if s.Naming.Preset == "" {
		s.Naming.Preset = NamingDefault
	}
	if !ValidChannel(s.Ytdlp.Channel) {
		s.Ytdlp.Channel = d.Ytdlp.Channel
	}
	if s.Ytdlp.AutoUpdate == nil {
		s.Ytdlp.AutoUpdate = d.Ytdlp.AutoUpdate
	}
	for _, c := range Channels {
		if _, ok := s.Ytdlp.AutoUpdate[c]; !ok {
			s.Ytdlp.AutoUpdate[c] = d.Ytdlp.AutoUpdate[c]
		}
	}
	if s.RateLimitCooldownMinutes <= 0 {
		s.RateLimitCooldownMinutes = d.RateLimitCooldownMinutes
	}
	if s.AgeRules == nil {
		s.AgeRules = d.AgeRules
	}
	if s.MinFreeSpaceMB < 0 {
		s.MinFreeSpaceMB = 0
	}
	// Settings stored before subscriptions existed have none of these.
	sub, ds := &s.Subscriptions, DefaultSubscriptionSettings()
	if *sub == (SubscriptionSettings{}) {
		*sub = ds
	}
	for _, f := range []struct{ v, d *int }{
		{&sub.IntervalMinutes, &ds.IntervalMinutes}, {&sub.CheckConcurrency, &ds.CheckConcurrency},
		{&sub.Window, &ds.Window}, {&sub.FullRescanDays, &ds.FullRescanDays}, {&sub.AutoQueueCap, &ds.AutoQueueCap},
		{&sub.BackgroundSlots, &ds.BackgroundSlots}, {&sub.UpcomingDays, &ds.UpcomingDays},
	} {
		if *f.v <= 0 {
			*f.v = *f.d
		}
	}
}

// Validate checks user-editable settings.
func (s *Settings) Validate() error {
	if s.MaxConcurrent < 1 || s.MaxConcurrent > 50 {
		return fmt.Errorf("maxConcurrent must be between 1 and 50")
	}
	if s.MaxRetries < 0 || s.MaxRetries > 20 {
		return fmt.Errorf("maxRetries must be between 0 and 20")
	}
	if !ValidContainer(s.Defaults.Container) {
		return fmt.Errorf("unknown container %q", s.Defaults.Container)
	}
	if _, ok := s.Presets[s.Defaults.Preset]; !ok {
		return fmt.Errorf("unknown default preset %q", s.Defaults.Preset)
	}
	for name, sort := range s.Presets {
		if !validName(name) {
			return fmt.Errorf("invalid preset name %q", name)
		}
		if err := ValidateSortString(sort); err != nil {
			return fmt.Errorf("preset %s: %w", name, err)
		}
	}
	if err := ValidateImpersonate(s.Defaults.Impersonate); err != nil {
		return err
	}
	switch s.Naming.Preset {
	case NamingDefault, NamingLegacy, NamingCustom:
	default:
		return fmt.Errorf("unknown naming preset %q", s.Naming.Preset)
	}
	if !ValidChannel(s.Ytdlp.Channel) {
		return fmt.Errorf("unknown yt-dlp channel %q", s.Ytdlp.Channel)
	}
	for ch, p := range s.Ytdlp.AutoUpdate {
		if !ValidChannel(ch) {
			return fmt.Errorf("unknown channel %q in autoUpdate", ch)
		}
		if p != UpdateOff && p != UpdateNotify && p != UpdateAuto {
			return fmt.Errorf("unknown auto-update policy %q", p)
		}
	}
	if u := s.Ytdlp.SmokeTestURL; u != "" {
		if err := ValidateURL(u); err != nil {
			return fmt.Errorf("smoke test URL: %w", err)
		}
	}
	if s.RateLimitCooldownMinutes < 1 || s.RateLimitCooldownMinutes > 24*60 {
		return fmt.Errorf("rateLimitCooldownMinutes must be between 1 and 1440")
	}
	if s.AutoPruneDays < 0 {
		return fmt.Errorf("autoPruneDays must not be negative")
	}
	seen := map[string]bool{DefaultLibrary: true}
	for _, l := range s.Libraries {
		if !validName(l.Name) || seen[l.Name] {
			return fmt.Errorf("invalid or duplicate library name %q", l.Name)
		}
		seen[l.Name] = true
		if !filepath.IsAbs(l.Path) {
			return fmt.Errorf("library %s: path must be absolute", l.Name)
		}
	}
	if !seen[s.Defaults.Library] {
		return fmt.Errorf("default library %q does not exist", s.Defaults.Library)
	}
	n := s.Notifications
	for _, u := range []string{n.WebhookURL, n.NtfyURL, n.GotifyURL, n.DiscordWebhookURL, s.Jellyfin.URL} {
		if u == "" {
			continue
		}
		if pu, err := url.Parse(u); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			return fmt.Errorf("invalid URL %q", u)
		}
	}
	for _, r := range s.AgeRules {
		if strings.TrimSpace(r.Match) == "" || strings.TrimSpace(r.Rating) == "" {
			return fmt.Errorf("age rules need a match and a rating")
		}
	}
	return s.Subscriptions.Validate()
}

func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == ' ' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// ValidContainer reports whether c is a supported merge container.
func ValidContainer(c string) bool { return c == "mkv" || c == "mp4" }

// ValidateSortString allows only the characters yt-dlp's -S syntax uses, so
// a preset can never smuggle in another option.
func ValidateSortString(s string) error {
	if strings.HasPrefix(s, "-") {
		return fmt.Errorf("sort string must not start with '-'")
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || strings.ContainsRune(",:+~._-", r)) {
			return fmt.Errorf("invalid character %q in sort string", r)
		}
	}
	return nil
}

// ValidateImpersonate checks an impersonation option: "", "any" or CLIENT[:OS].
func ValidateImpersonate(s string) error {
	if s == "" || s == "any" {
		return nil
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || strings.ContainsRune(".:-_", r)) {
			return fmt.Errorf("invalid impersonate target %q", s)
		}
	}
	return nil
}

// ValidateURL accepts only http(s) URLs that cannot be mistaken for options
// (§11 argument injection).
func ValidateURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url is required")
	}
	if strings.HasPrefix(raw, "-") {
		return fmt.Errorf("url must not start with '-'")
	}
	if strings.ContainsAny(raw, "\x00\r\n") || strings.TrimSpace(raw) != raw {
		return fmt.Errorf("url contains invalid characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http(s) URLs are supported")
	}
	if u.Host == "" {
		return fmt.Errorf("url has no host")
	}
	return nil
}

// SiteRule customizes options for matching hosts (§7.2).
type SiteRule struct {
	ID            int64   `json:"id"`
	Position      int     `json:"position"`
	Pattern       string  `json:"pattern"`
	Impersonate   string  `json:"impersonate,omitempty"`
	CookieProfile string  `json:"cookieProfile,omitempty"`
	Preset        string  `json:"preset,omitempty"`
	MaxConcurrent int     `json:"maxConcurrent,omitempty"`
	LimitRate     string  `json:"limitRate,omitempty"`
	SleepRequests float64 `json:"sleepRequests,omitempty"`
	SleepInterval float64 `json:"sleepInterval,omitempty"`
	Library       string  `json:"library,omitempty"`
}

// Validate checks a site rule.
func (r *SiteRule) Validate() error {
	r.Pattern = strings.ToLower(strings.TrimSpace(r.Pattern))
	if r.Pattern == "" || strings.ContainsAny(r.Pattern, "/ ") {
		return fmt.Errorf("pattern must be a host glob such as *.example.com")
	}
	if _, err := path.Match(r.Pattern, "x"); err != nil {
		return fmt.Errorf("invalid pattern: %w", err)
	}
	if err := ValidateImpersonate(r.Impersonate); err != nil {
		return err
	}
	if r.MaxConcurrent < 0 {
		return fmt.Errorf("maxConcurrent must not be negative")
	}
	if r.LimitRate != "" {
		for _, c := range r.LimitRate {
			if !(c >= '0' && c <= '9' || strings.ContainsRune(".KMGkmg", c)) {
				return fmt.Errorf("invalid limit rate %q (e.g. 5M)", r.LimitRate)
			}
		}
	}
	if r.SleepRequests < 0 || r.SleepInterval < 0 {
		return fmt.Errorf("sleep values must not be negative")
	}
	return nil
}

// Matches reports whether the rule applies to host.
func (r *SiteRule) Matches(host string) bool {
	host = strings.ToLower(host)
	p := r.Pattern
	if strings.ContainsAny(p, "*?[") {
		if ok, _ := path.Match(p, host); ok {
			return true
		}
		// "*.example.com" also covers the bare domain.
		if strings.HasPrefix(p, "*.") && host == p[2:] {
			return true
		}
		return false
	}
	return host == p || strings.HasSuffix(host, "."+p)
}

// MatchRule returns the first matching rule for host.
func MatchRule(rules []SiteRule, host string) *SiteRule {
	for i := range rules {
		if rules[i].Matches(host) {
			return &rules[i]
		}
	}
	return nil
}
