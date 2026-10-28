package manager

import (
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/arcticshift"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/native"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/subscriptions"
)

// redditState holds the Reddit client and providers (Reddit concept §5,
// §6). They are built on first use and rebuilt when the proxy changes.
type redditState struct {
	mu     sync.Mutex
	proxy  string
	client *browser.Client
	native *native.Provider
	arctic *arcticshift.Provider
	// test hooks: trust these roots and dial here (fakereddit).
	testOpts *browser.Options
}

// redditProviders returns the providers by name, building them if needed.
func (m *Manager) redditProviders() map[string]reddit.Provider {
	s := m.Settings.Settings().Reddit
	r := &m.reddit
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client == nil || r.proxy != s.Proxy {
		if r.client != nil {
			r.client.Close()
		}
		opts := browser.Options{Proxy: s.Proxy, JarPath: filepath.Join(m.dataDir, "reddit", "cookies.json")}
		if r.testOpts != nil {
			opts.RootCAs, opts.Dial, opts.Hosts = r.testOpts.RootCAs, r.testOpts.Dial, r.testOpts.Hosts
		}
		c, err := browser.New(opts)
		if err != nil {
			// The proxy was validated with the settings; fall back to a
			// direct client rather than failing every check.
			m.logf("reddit: %v", err)
			opts.Proxy = ""
			c, _ = browser.New(opts)
		}
		r.client, r.proxy = c, s.Proxy
		r.native = native.New(c)
		r.arctic = arcticshift.New(c)
		if r.testOpts != nil {
			r.arctic.MinInterval = 0
		}
	}
	if r.testOpts != nil {
		r.native.PageDelay = nil
	} else {
		delay := time.Duration(s.PageDelaySeconds) * time.Second
		r.native.PageDelay = func() time.Duration {
			return delay + time.Duration(rand.Float64()*0.4*float64(delay)) - delay/5
		}
	}
	return map[string]reddit.Provider{config.RedditNative: r.native, config.RedditArcticShift: r.arctic}
}

// redditChain returns the provider chain: a subscription's override
// ("native,arcticshift") or the global order.
func (m *Manager) redditChain(override string, logf func(string, ...any)) *reddit.Chain {
	s := m.Settings.Settings()
	names := s.Reddit.Providers
	if override = strings.TrimSpace(override); override != "" {
		names = strings.Split(override, ",")
	}
	all := m.redditProviders()
	var ps []reddit.Provider
	for _, n := range names {
		if p := all[strings.TrimSpace(n)]; p != nil {
			ps = append(ps, p)
		}
	}
	return &reddit.Chain{Providers: ps, Logf: logf,
		Cooldown:        func(site string) time.Time { return m.Sched.Status().Cooldowns[site] },
		SetCooldown:     m.Sched.SetCooldown,
		DefaultCooldown: time.Duration(s.RateLimitCooldownMinutes) * time.Minute}
}

// redditAccess gives the pipeline the provider chain and the client for
// media downloads.
type redditAccess struct{ m *Manager }

func (a redditAccess) Chain(providers string, logf func(string, ...any)) *reddit.Chain {
	return a.m.redditChain(providers, logf)
}

func (a redditAccess) Client() *browser.Client {
	a.m.redditProviders()
	a.m.reddit.mu.Lock()
	defer a.m.reddit.mu.Unlock()
	return a.m.reddit.client
}

// listerFor picks the lister of a subscription source or preview URL.
func (m *Manager) listerFor(u string) subscriptions.Lister {
	if _, ok := reddit.Parse(u); ok {
		return &subscriptions.RedditLister{Chain: m.redditChain,
			Settings: func() config.RedditSettings { return m.Settings.Settings().Reddit }}
	}
	return subscriptions.YtdlpLister{}
}

// closeReddit releases the Reddit connections.
func (m *Manager) closeReddit() {
	m.reddit.mu.Lock()
	defer m.reddit.mu.Unlock()
	if m.reddit.client != nil {
		m.reddit.client.Close()
		m.reddit.client = nil
	}
}
