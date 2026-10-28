package reddit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

type stubProvider struct {
	name, site string
	err        *Error
	calls      int
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Site() string { return s.site }
func (s *stubProvider) List(context.Context, Query) (*Page, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &Page{Posts: []Post{{ID: s.name}}}, nil
}
func (s *stubProvider) Post(_ context.Context, id, _ string) (*Post, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &Post{ID: id}, nil
}

func TestChain(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	cooldowns := map[string]time.Time{}
	newChain := func(ps ...Provider) *Chain {
		return &Chain{Providers: ps, Now: func() time.Time { return now }, DefaultCooldown: 30 * time.Minute,
			Cooldown: func(s string) time.Time { return cooldowns[s] }, SetCooldown: func(s string, u time.Time) { cooldowns[s] = u }}
	}
	ctx := context.Background()

	// Blocked falls through to the next provider.
	native := &stubProvider{name: "native", site: "reddit.com", err: &Error{Class: ytdlp.ClassBlocked, Reason: "blocked", Hint: HintBlocked}}
	arctic := &stubProvider{name: "arcticshift", site: "photon-reddit.com"}
	pg, name, err := newChain(native, arctic).List(ctx, Query{})
	if err != nil || name != "arcticshift" || pg.Posts[0].ID != "arcticshift" {
		t.Fatalf("fall through: %v %s", err, name)
	}
	post, name, err := newChain(native, arctic).Post(ctx, "abc", "")
	if err != nil || name != "arcticshift" || post.Provider != "arcticshift" {
		t.Fatalf("post: %v %s %+v", err, name, post)
	}

	// A permanent error stops the chain.
	native.err = &Error{Class: ytdlp.ClassPermanent, Reason: "banned"}
	arctic.calls = 0
	if _, _, err := newChain(native, arctic).List(ctx, Query{}); err == nil || err.Class != ytdlp.ClassPermanent || arctic.calls != 0 {
		t.Fatalf("permanent: %+v, arctic calls %d", err, arctic.calls)
	}

	// A rate limit sets the cooldown of the provider's site, and the
	// provider is skipped while it lasts. All failing: the first error,
	// with the providers tried in the hint.
	native.err = &Error{Class: ytdlp.ClassRateLimited, Reason: "429", RetryAfter: 2 * time.Minute}
	arctic.err = &Error{Class: ytdlp.ClassTransient, Reason: "5xx"}
	_, _, err = newChain(native, arctic).List(ctx, Query{})
	if err == nil || err.Class != ytdlp.ClassRateLimited || !cooldowns["reddit.com"].Equal(now.Add(2*time.Minute)) ||
		!strings.Contains(err.Hint, "Tried: native, arcticshift") {
		t.Fatalf("all failed: %+v %v", err, cooldowns)
	}
	native.calls = 0
	_, _, err = newChain(native, arctic).List(ctx, Query{})
	if native.calls != 0 || err.Class != ytdlp.ClassRateLimited {
		t.Fatalf("cooldown not respected: %d %+v", native.calls, err)
	}

	// An unsupported request is skipped without counting as an error.
	cooldowns = map[string]time.Time{}
	native.err = nil
	arctic.err = &Error{Class: ytdlp.ClassPermanent, Reason: "time only", Unsupported: true}
	if _, name, err := newChain(arctic, native).List(ctx, Query{}); err != nil || name != "native" {
		t.Fatalf("unsupported: %v %s", err, name)
	}
	if _, _, err := newChain(arctic).List(ctx, Query{}); err == nil || err.Reason != "time only" {
		t.Fatalf("only unsupported: %+v", err)
	}
	if _, _, err := newChain().List(ctx, Query{}); err == nil || err.Hint != HintNoProviders {
		t.Fatalf("no providers: %+v", err)
	}
}

// A blocked provider is paused: the next requests skip it and go to the
// next provider, and the skip keeps the cause.
func TestChainPausesBlockedProvider(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	cooldowns, causes := map[string]time.Time{}, map[string]*Error{}
	native := &stubProvider{name: "native", site: "reddit-native", err: &Error{Class: ytdlp.ClassBlocked, Reason: "a JavaScript check", Hint: HintChallenge}}
	arctic := &stubProvider{name: "arcticshift", site: "arctic-shift"}
	chain := &Chain{Providers: []Provider{native, arctic}, Now: func() time.Time { return now }, BlockedCooldown: 30 * time.Minute,
		Cooldown: func(s string) time.Time { return cooldowns[s] }, SetCooldown: func(s string, u time.Time) { cooldowns[s] = u },
		OnPause: func(s string, e *Error) { causes[s] = e }, PauseCause: func(s string) *Error { return causes[s] }}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, name, err := chain.Post(ctx, "abc", ""); err != nil || name != "arcticshift" {
			t.Fatalf("post %d: %v %s", i, err, name)
		}
	}
	if native.calls != 1 || !cooldowns["reddit-native"].Equal(now.Add(30*time.Minute)) {
		t.Fatalf("native called %d times, paused until %v", native.calls, cooldowns["reddit-native"])
	}
	// With Arctic Shift down too, the cause of the pause stays the reason,
	// and the request is worth retrying once Arctic Shift is back.
	arctic.err = &Error{Class: ytdlp.ClassTransient, Reason: "HTTP 500"}
	_, _, err := chain.Post(ctx, "abc", "")
	if err == nil || err.Class != ytdlp.ClassTransient ||
		!strings.Contains(err.Reason, "paused until 12:30 after: a JavaScript check (arcticshift: HTTP 500)") ||
		!strings.HasPrefix(err.Hint, HintChallenge) {
		t.Fatalf("skipped cause: %+v", err)
	}
	arctic.err = &Error{Class: ytdlp.ClassAuthRequired, Reason: "withheld"}
	if _, _, err := chain.Post(ctx, "abc", ""); err == nil || err.Class != ytdlp.ClassBlocked {
		t.Fatalf("nothing temporary: %+v", err)
	}
}

// One provider can't read the page, the other is down for a while: the
// request fails with the first reason, but as temporary, so an item is
// retried and gets through once the other provider is back.
func TestChainTemporaryFailure(t *testing.T) {
	native := &stubProvider{name: "native", site: "reddit-native", err: &Error{Class: ytdlp.ClassExtractorBroken,
		Reason: "no posts found in Reddit's page", Hint: HintLayout}}
	arctic := &stubProvider{name: "arcticshift", site: "arctic-shift", err: &Error{Class: ytdlp.ClassTransient, Reason: "HTTP 503"}}
	chain := &Chain{Providers: []Provider{native, arctic}}
	_, _, err := chain.Post(context.Background(), "abc", "")
	if err == nil || err.Class != ytdlp.ClassTransient || err.Reason != "no posts found in Reddit's page (arcticshift: HTTP 503)" ||
		!strings.HasPrefix(err.Hint, HintLayout) {
		t.Fatalf("%+v", err)
	}
	arctic.err = &Error{Class: ytdlp.ClassRateLimited, Reason: "slow down", RetryAfter: time.Minute}
	if _, _, err := chain.Post(context.Background(), "abc", ""); err.Class != ytdlp.ClassRateLimited || err.RetryAfter != time.Minute {
		t.Fatalf("rate limit: %+v", err)
	}
}
