package subscriptions

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// A backfill that failed for a while is repeated after 5 minutes, then
// doubling up to 4 hours, but not before the failing provider's pause ends.
func TestBackfillRetry(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		failures int
		pause    time.Duration
		want     time.Duration
	}{
		{1, 0, 5 * time.Minute},
		{3, 0, 20 * time.Minute},
		{20, 0, 4 * time.Hour},
		{1, 30 * time.Minute, 30 * time.Minute},
	} {
		got := backfillRetry(now, &BackfillResult{Next: &store.Backfill{Failures: c.failures}, RetryAfter: c.pause})
		if got.Sub(now) != c.want {
			t.Errorf("%d failures, pause %v: %v", c.failures, c.pause, got.Sub(now))
		}
	}
}

// timeProvider lists its posts by time, with an inclusive before, like
// Arctic Shift.
type timeProvider struct{ posts []reddit.Post } // newest first

func (p *timeProvider) Name() string { return "arcticshift" }
func (p *timeProvider) Site() string { return "arctic-shift" }
func (p *timeProvider) Post(context.Context, string, string) (*reddit.Post, error) {
	return nil, &reddit.Error{Reason: "no"}
}
func (p *timeProvider) List(_ context.Context, q reddit.Query) (*reddit.Page, error) {
	out := &reddit.Page{}
	for _, ps := range p.posts {
		if (q.Before.IsZero() || !ps.Created.After(q.Before)) && len(out.Posts) < q.Limit {
			out.Posts = append(out.Posts, ps)
		}
	}
	out.End = len(out.Posts) < q.Limit
	return out, nil
}

// Continue lists the second of the oldest post loaded again: the posts of
// it loaded before are not loaded or counted twice, also when that second
// holds more posts than one run loads.
func TestBackfillContinueBoundary(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var posts []reddit.Post
	for i := 0; i < 12; i++ {
		at := base.Add(-time.Duration(i) * time.Hour)
		if i >= 3 && i < 9 {
			at = base.Add(-3 * time.Hour) // six posts in one second
		}
		posts = append(posts, reddit.Post{ID: fmt.Sprintf("p%02d", i), Kind: reddit.KindImage, Created: at,
			Media: []reddit.Media{{URL: fmt.Sprintf("https://i.redd.it/m%02d.png", i), MediaID: fmt.Sprintf("m%02d", i)}}})
	}
	prov := &timeProvider{posts: posts}
	r := &RedditLister{Chain: func(string, func(string, ...any)) *reddit.Chain {
		return &reddit.Chain{Providers: []reddit.Provider{prov}}
	}, Settings: func() config.RedditSettings { return config.RedditSettings{BackfillMax: 5} }}
	b := store.Backfill{ID: 1, Kind: store.BackfillRange, Pending: true}
	seen := map[string]int{}
	for run := 0; run < 5; run++ {
		res := r.Backfill(context.Background(), BackfillRequest{URL: "https://www.reddit.com/r/example/", Backfill: b, Timeout: time.Minute})
		if res.Err != nil {
			t.Fatalf("run %d: %+v", run, res.Err)
		}
		for _, e := range res.Listing.Entries {
			seen[e.URL]++
		}
		if res.Next == nil {
			break
		}
		b = *res.Next
		b.Pending = true
	}
	if len(seen) != 12 {
		t.Fatalf("%d posts loaded: %v", len(seen), seen)
	}
	for u, n := range seen {
		if n != 1 {
			t.Fatalf("%s loaded %d times", u, n)
		}
	}
	if b.Loaded != 10 { // the last run's 2 aren't in b
		t.Fatalf("loaded %d", b.Loaded)
	}
}

// The description shows the last day of a range, not the exclusive end.
func TestBackfillDesc(t *testing.T) {
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	for want, b := range map[string]store.Backfill{
		"posts of 2026-07-01 – 2026-07-31":  {Kind: store.BackfillRange, From: day("2026-07-01"), To: day("2026-08-01")},
		"posts of the beginning – now":      {Kind: store.BackfillRange},
		"top 50 of 2023-01-01 – 2023-12-31": {Kind: store.BackfillRangeTop, N: 50, From: day("2023-01-01"), To: day("2024-01-01")},
		"top 100 of all time":               {Kind: store.BackfillTop, N: 100, Period: "all"},
	} {
		if got := BackfillDesc(&b); got != want {
			t.Errorf("%s: %s", want, got)
		}
	}
}
