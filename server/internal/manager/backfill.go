package manager

import (
	"context"
	"sort"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/subscriptions"
)

// Loading older posts of a Reddit subscription (Reddit concept §7.7): the
// request is stored on the subscription, and its next check runs it.

// BackfillInput is POST /subscriptions/{id}/backfill.
type BackfillInput struct {
	Kind   string `json:"kind"`             // top, range, rangeTop
	Period string `json:"period,omitempty"` // top: hour … all
	N      int    `json:"n,omitempty"`      // top, rangeTop
	From   string `json:"from,omitempty"`   // range, rangeTop: YYYY-MM-DD; empty = the beginning
	To     string `json:"to,omitempty"`     // YYYY-MM-DD (the whole day); empty = now
	// Download queues the loaded posts at once, in the backlog band.
	Download bool `json:"download,omitempty"`
	// Continue resumes a range that stopped at backfillMax.
	Continue bool `json:"continue,omitempty"`
}

// BackfillView is a stored request in the API.
type BackfillView struct {
	Kind     string `json:"kind"`
	Period   string `json:"period,omitempty"`
	N        int    `json:"n,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Download bool   `json:"download,omitempty"`
	// Pending: the next check runs it. Otherwise it stopped at the limit
	// (CanContinue) or failed (Error).
	Pending     bool   `json:"pending"`
	CanContinue bool   `json:"canContinue,omitempty"`
	Loaded      int    `json:"loaded"`
	Total       int    `json:"total,omitempty"` // about; 0 = unknown
	Error       string `json:"error,omitempty"`
	Description string `json:"description"`
}

func backfillView(b *store.Backfill) *BackfillView {
	if b == nil {
		return nil
	}
	v := &BackfillView{Kind: b.Kind, Period: b.Period, N: b.N, Download: b.Download, Pending: b.Pending, Loaded: b.Loaded,
		Total: b.Total, Error: b.Error, Description: subscriptions.BackfillDesc(b)}
	if !b.From.IsZero() {
		v.From = b.From.UTC().Format(time.DateOnly)
	}
	if !b.To.IsZero() {
		v.To = b.To.UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	}
	v.CanContinue = !b.Pending && b.Error == "" && !b.Before.IsZero()
	return v
}

// BackfillCounts is the answer of POST /subscriptions/{id}/backfill/preview.
type BackfillCounts struct {
	Years []reddit.YearCount `json:"years"`
	Total int                `json:"total"`
	// Known is false when Arctic Shift could not count (its aggregation
	// times out for very large subreddits); the backfill still works.
	Known       bool `json:"known"`
	BackfillMax int  `json:"backfillMax"`
}

// redditTarget returns the subreddit or user a subscription follows: its
// first Reddit listing source, as a listing of new posts.
func redditTarget(sub *store.Subscription) (reddit.Target, bool) {
	for _, src := range sub.Sources {
		if t, ok := reddit.Parse(src.Canonical); ok && t.IsListing() {
			t.Sort, t.Period = "", ""
			return t, true
		}
	}
	return reddit.Target{}, false
}

// dayRange parses the from and to days of a request: from midnight of
// from to the end of to (UTC).
func dayRange(from, to string) (time.Time, time.Time, error) {
	var f, t time.Time
	var err error
	if from != "" {
		if f, err = time.Parse(time.DateOnly, from); err != nil {
			return f, t, invalid("from must be a date (YYYY-MM-DD)")
		}
	}
	if to != "" {
		if t, err = time.Parse(time.DateOnly, to); err != nil {
			return f, t, invalid("to must be a date (YYYY-MM-DD)")
		}
		t = t.AddDate(0, 0, 1)
	}
	if !f.IsZero() && !t.IsZero() && !f.Before(t) {
		return f, t, invalid("from must be before to")
	}
	return f, t, nil
}

// RequestBackfill stores a backfill request, or continues one that
// stopped at backfillMax; the next check, due at once, runs it.
func (m *Manager) RequestBackfill(ctx context.Context, id int64, in BackfillInput) (*SubscriptionView, error) {
	sub, err := m.Store.GetSubscription(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, ok := redditTarget(sub); !ok {
		return nil, invalid("older posts can only be loaded for Reddit subreddits and users")
	}
	now := time.Now()
	var b *store.Backfill
	if in.Continue {
		if sub.Backfill == nil || sub.Backfill.Pending || sub.Backfill.Before.IsZero() {
			return nil, invalid("there is nothing to continue")
		}
		b = sub.Backfill
		b.Pending, b.Error = true, ""
	} else {
		b = &store.Backfill{ID: now.UnixMilli(), Kind: in.Kind, Download: in.Download, Pending: true}
		switch in.Kind {
		case store.BackfillTop:
			if !reddit.ValidPeriod(in.Period) {
				return nil, invalid("period must be hour, day, week, month, year or all")
			}
			b.Period = in.Period
		case store.BackfillRange, store.BackfillRangeTop:
			if b.From, b.To, err = dayRange(in.From, in.To); err != nil {
				return nil, err
			}
			if in.Kind == store.BackfillRangeTop && (b.From.IsZero() || b.To.IsZero()) {
				return nil, invalid("the top of a time range needs from and to")
			}
		default:
			return nil, invalid("kind must be top, range or rangeTop")
		}
		if in.Kind != store.BackfillRange {
			if in.N < 1 || in.N > reddit.MaxListing {
				return nil, invalid("n must be between 1 and %d", reddit.MaxListing)
			}
			b.N = in.N
		}
		if in.Kind != store.BackfillTop {
			// Count first; the top of a range sorts all of it, so it is
			// only offered up to backfillMax posts.
			if c := m.backfillCounts(ctx, sub, b.From, b.To); c.Known {
				b.Total = c.Total
				if in.Kind == store.BackfillRangeTop && c.Total > c.BackfillMax {
					return nil, invalid("about %d posts in this range, more than %d (backfillMax): narrow it", c.Total, c.BackfillMax)
				}
			}
		}
	}
	if err := m.Store.SetBackfill(ctx, id, b, now); err != nil {
		return nil, err
	}
	m.publishSub(id)
	m.subs.loop.Wake()
	return m.GetSubscription(ctx, id)
}

// CancelBackfill removes a backfill request.
func (m *Manager) CancelBackfill(ctx context.Context, id int64) (*SubscriptionView, error) {
	if err := m.Store.SetBackfill(ctx, id, nil, time.Time{}); err != nil {
		return nil, err
	}
	m.publishSub(id)
	return m.GetSubscription(ctx, id)
}

// PreviewBackfill counts the posts of a time range per year, with the
// subscription's media kind and NSFW filters.
func (m *Manager) PreviewBackfill(ctx context.Context, id int64, from, to string) (*BackfillCounts, error) {
	sub, err := m.Store.GetSubscription(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, ok := redditTarget(sub); !ok {
		return nil, invalid("older posts can only be loaded for Reddit subreddits and users")
	}
	f, t, err := dayRange(from, to)
	if err != nil {
		return nil, err
	}
	return m.backfillCounts(ctx, sub, f, t), nil
}

// backfillCounts sums Arctic Shift's counts of the searches a range would
// run (one per URL prefix of the media kinds).
func (m *Manager) backfillCounts(ctx context.Context, sub *store.Subscription, from, to time.Time) *BackfillCounts {
	out := &BackfillCounts{Years: []reddit.YearCount{}, BackfillMax: m.Settings.Settings().Reddit.BackfillMax}
	t, _ := redditTarget(sub)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	chain := m.redditChain(sub.Provider, func(string, ...any) {})
	years := map[int]int{}
	for _, f := range subscriptions.ServerFilters(sub.Filters) {
		ys, _, err := chain.Count(ctx, reddit.Query{Target: t, After: from, Before: to, ByTime: true, URLPrefix: f.Prefix,
			Over18: f.Over18})
		if err != nil {
			return out
		}
		for _, y := range ys {
			years[y.Year] += y.Count
		}
	}
	for y := range years {
		out.Years = append(out.Years, reddit.YearCount{Year: y, Count: years[y]})
		out.Total += years[y]
	}
	sort.Slice(out.Years, func(i, j int) bool { return out.Years[i].Year < out.Years[j].Year })
	out.Known = true
	return out
}
