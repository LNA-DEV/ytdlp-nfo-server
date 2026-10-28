package subscriptions

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/joblog"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp/versions"
)

// stubLister returns `window` unknown entries and is exhausted at limit.
type stubLister struct {
	limit   int
	urls    []string
	windows []int
}

func (s *stubLister) List(_ context.Context, req ListRequest) (*ListResult, *ytdlp.Classification) {
	s.urls = append(s.urls, req.URL)
	s.windows = append(s.windows, req.Window)
	n := min(req.Window, s.limit)
	l := &ytdlp.Listing{IsPlaylist: true}
	for i := range n {
		l.Entries = append(l.Entries, ytdlp.ListEntry{ExtractorKey: "Stub", ID: fmt.Sprint(i)})
	}
	return &ListResult{Listing: l, Exhausted: req.Window >= s.limit}, nil
}

// The check uses the lister of the source's URL, doubles the window while
// no known entry shows up, and stops once the lister is exhausted.
func TestListSourceStopsWhenListerIsExhausted(t *testing.T) {
	logs, err := joblog.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubLister{limit: 150}
	l := NewLoop(Deps{Logs: logs, ListerFor: func(string) Lister { return stub }})
	now := time.Now()
	src := &store.Source{ID: 1, Canonical: "https://example.com/list", FirstOKAt: now, LastFullAt: now,
		NewestFirst: true, OrderKnown: true}
	sub := &store.Subscription{ID: 1, Listing: store.ListingAuto, Sources: []*store.Source{src}}
	settings := config.Settings{Subscriptions: config.DefaultSubscriptionSettings()}
	r := l.listSource(context.Background(), sub, src, versions.Binary{}, ytdlp.CallOptions{}, settings,
		map[string]bool{"stub known": true}, false, logs.Get(1))
	if r.Err != nil || r.Complete || r.Listing == nil || len(r.Listing.Entries) != 150 {
		t.Fatalf("result: %+v", r)
	}
	if fmt.Sprint(stub.windows) != "[50 100 200]" || stub.urls[0] != src.Canonical {
		t.Fatalf("windows %v, urls %v", stub.windows, stub.urls)
	}
}
