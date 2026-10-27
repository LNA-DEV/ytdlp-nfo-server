package subscriptions

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// world keeps the stored entries of one subscription between checks, as
// the commit transaction does.
type world struct {
	sub     *store.Subscription
	entries []*store.Entry
	nextID  int64
	now     time.Time
}

func newApplyWorld(mode string, tabs ...string) *world {
	w := &world{sub: &store.Subscription{ID: 1, Name: "Chan", Mode: mode, Enabled: true}, now: t0}
	for i, tab := range tabs {
		w.sub.Sources = append(w.sub.Sources, &store.Source{ID: int64(i + 1), SubscriptionID: 1, Tab: tab,
			URL: "https://fake.test/pl/" + tab, Canonical: "https://fake.test/pl/" + tab})
	}
	return w
}

// listing builds a listing of video ids; "id@20240101" sets the upload
// date, "id!post_live" the live status, "id$subscriber_only" availability.
func listing(ids ...string) *ytdlp.Listing {
	l := &ytdlp.Listing{ID: "UCchan", ChannelID: "UCchan", Channel: "Chan", IsPlaylist: true}
	for _, spec := range ids {
		e := ytdlp.ListEntry{ExtractorKey: "Fake", Duration: 300}
		id := spec
		if i := strings.IndexAny(spec, "@!$"); i >= 0 {
			id = spec[:i]
		}
		for _, f := range strings.FieldsFunc(spec[len(id):], func(r rune) bool { return r == ',' }) {
			switch {
			case strings.HasPrefix(f, "@"):
				e.UploadDate = f[1:]
			case strings.HasPrefix(f, "!"):
				e.LiveStatus = f[1:]
			case strings.HasPrefix(f, "$"):
				e.Availability = f[1:]
			}
		}
		e.ID, e.Title, e.URL = id, "Title "+id, "https://fake.test/v/"+id
		l.Entries = append(l.Entries, e)
	}
	return l
}

func ok(src int64, complete bool, l *ytdlp.Listing) SourceResult {
	return SourceResult{SourceID: src, Listing: l, Complete: complete}
}

func failed(src int64, class ytdlp.Class, reason string) SourceResult {
	return SourceResult{SourceID: src, Err: &ytdlp.Classification{Class: class, Reason: reason}}
}

func (w *world) check(archived map[string]bool, rs ...SourceResult) *Output {
	return w.checkWith(func(in *Input) { in.Archived = archived }, rs...)
}

func (w *world) checkWith(mod func(*Input), rs ...SourceResult) *Output {
	in := Input{Now: w.now, Sub: w.sub, Entries: w.entries, Result: &Result{Sources: rs},
		Settings: config.DefaultSubscriptionSettings(), Jitter: func() float64 { return 0 }}
	if mod != nil {
		mod(&in)
	}
	out := Apply(in)
	for _, e := range out.Insert {
		w.nextID++
		e.ID = w.nextID
		w.entries = append(w.entries, e)
	}
	w.now = w.now.Add(6 * time.Hour)
	return out
}

func (w *world) entry(key string) *store.Entry {
	for _, e := range w.entries {
		if e.Key == key {
			return e
		}
	}
	return nil
}

func ids(es []*store.Entry) string {
	var out []string
	for _, e := range es {
		out = append(out, strings.TrimPrefix(e.Key, "fake "))
	}
	return strings.Join(out, ",")
}

func TestApplyFirstListingIsBacklogThenNewAreQueued(t *testing.T) {
	w := newApplyWorld(store.ModeAuto, TabVideos)
	var first []string
	for i := 30; i >= 1; i-- {
		first = append(first, fmt.Sprintf("v%d", i))
	}
	out := w.check(nil, ok(1, true, listing(first...)))
	if len(out.Insert) != 30 || out.Summary.Backlog != 30 || len(out.Auto) != 0 {
		t.Fatalf("first listing: insert %d backlog %d auto %d", len(out.Insert), out.Summary.Backlog, len(out.Auto))
	}
	if out.Insert[0].Key != "fake v1" || !out.Insert[0].Backlog {
		t.Fatalf("insert order must be chronological: %s", out.Insert[0].Key)
	}
	src := w.sub.Sources[0]
	if src.FirstOKAt != t0 || src.LastFullAt != t0 || w.sub.State != store.SubIdle || w.sub.Kind != "channel" || w.sub.Uploader != "Chan" {
		t.Fatalf("state after the first check: %+v %+v", src, w.sub)
	}
	if w.sub.NextCheckAt != t0.Add(6*time.Hour) {
		t.Fatalf("next check %v", w.sub.NextCheckAt)
	}

	// Second check (windowed): 2 new videos, one already in the archive.
	out = w.check(map[string]bool{"fake v33": true}, ok(1, false, listing(append([]string{"v33", "v32", "v31"}, first[:5]...)...)))
	if out.Summary.New != 3 || ids(out.Auto) != "v31,v32" || len(out.Update) != 5 {
		t.Fatalf("second check: new %d auto %s update %d", out.Summary.New, ids(out.Auto), len(out.Update))
	}
	if w.entry("fake v31").Backlog || out.Summary.Gone != 0 {
		t.Fatal("a windowed check must not mark entries gone, new entries are not backlog")
	}

	// A complete listing without v1 and v31–v33: gone. v1 reappears: gone
	// cleared.
	out = w.check(nil, ok(1, true, listing(first[:29]...)))
	if out.Summary.Gone != 4 || w.entry("fake v1").GoneAt.IsZero() {
		t.Fatalf("gone: %d", out.Summary.Gone)
	}
	w.check(nil, ok(1, false, listing("v1")))
	if !w.entry("fake v1").GoneAt.IsZero() {
		t.Fatal("reappeared entry still gone")
	}
}

func TestApplyManualModeAndFilters(t *testing.T) {
	w := newApplyWorld(store.ModeManual, "")
	w.sub.Filters = store.Filters{MinDuration: 400}
	w.check(nil, ok(1, true, listing("a")))
	l := listing("a", "b", "c")
	l.Entries[2].Duration = 0 // unknown: passes
	l.Entries = append(l.Entries, ytdlp.ListEntry{ExtractorKey: "FakePlaylist", ID: "pl", URL: "https://fake.test/pl/x", Nested: true})
	out := w.check(nil, ok(1, true, l))
	if len(out.Auto) != 0 || out.Summary.New != 3 || out.Summary.Filtered != 1 {
		t.Fatalf("manual: auto %d new %d filtered %d", len(out.Auto), out.Summary.New, out.Summary.Filtered)
	}
	if b := w.entry("fake b"); !b.Ignored || b.IgnoreReason != "filter: shorter than 6:40" {
		t.Fatalf("filtered entry: %+v", b)
	}
	if c := w.entry("fake c"); c.Ignored {
		t.Fatal("unknown duration must pass")
	}
	if p := w.entry("fakeplaylist pl"); p == nil || p.IgnoreReason != ReasonNotAVideo {
		t.Fatalf("nested entry: %+v", p)
	}
}

func TestApplyCap(t *testing.T) {
	w := newApplyWorld(store.ModeAuto, TabVideos)
	w.check(nil, ok(1, true, listing("a")))
	var many []string
	for i := 0; i < 26; i++ {
		many = append(many, fmt.Sprint("n", i))
	}
	out := w.check(nil, ok(1, false, listing(append(many, "a")...)))
	if len(out.Auto) != 0 || !out.CapHit || w.sub.Warning != "Found 26 new videos (limit 25). Review them before downloading." {
		t.Fatalf("cap: auto %d hit %v warning %q", len(out.Auto), out.CapHit, w.sub.Warning)
	}
	out = w.check(nil, ok(1, false, listing(append([]string{"x"}, many...)...)))
	if ids(out.Auto) != "x" || w.sub.Warning != "" || out.CapHit {
		t.Fatalf("within cap again: %s %q", ids(out.Auto), w.sub.Warning)
	}
	// The inbox entries of the capped check were never queued (S3).
	if e := w.entry("fake n0"); e.ItemID != 0 || e.Backlog {
		t.Fatalf("capped entry: %+v", e)
	}
}

func TestApplyUpcoming(t *testing.T) {
	w := newApplyWorld(store.ModeAuto, TabVideos)
	w.check(nil, ok(1, true, listing("a")))
	out := w.check(nil, ok(1, false, listing("p!is_upcoming", "s!post_live", "a")))
	if len(out.Auto) != 0 {
		t.Fatalf("upcoming queued: %s", ids(out.Auto))
	}
	out = w.check(nil, ok(1, false, listing("p!is_upcoming", "s!post_live", "a")))
	if len(out.Requeue)+len(out.Auto) != 0 {
		t.Fatal("still upcoming")
	}
	out = w.check(nil, ok(1, false, listing("p!is_upcoming", "s!was_live", "a")))
	if ids(out.Requeue) != "s" || len(out.Auto) != 0 {
		t.Fatalf("finished stream: requeue %s", ids(out.Requeue))
	}

	// Classifier path: the item failed with "not available yet".
	nya := w.entry("fake p")
	nya.LiveStatus = ""
	nya.ItemID, nya.ItemState, nya.ItemError = 7, store.ItemFailed, ytdlp.ReasonNotYetAvailable
	l := listing("a")
	out = w.check(nil, ok(1, false, l))
	if ids(out.Requeue) != "p" {
		t.Fatalf("classifier path: %s", ids(out.Requeue))
	}
	w.now = nya.FirstSeenAt.AddDate(0, 0, 8)
	if out = w.check(nil, ok(1, false, l)); len(out.Requeue) != 0 {
		t.Fatal("re-queued after upcomingDays")
	}
	// Any other failure is never queued again (S3).
	other := w.entry("fake a")
	other.ItemID, other.ItemState, other.ItemError = 8, store.ItemFailed, "video is private"
	if out = w.check(nil, ok(1, false, listing("a"))); len(out.Requeue) != 0 {
		t.Fatal("failed entry re-queued")
	}
}

func TestApplyNeedsCookies(t *testing.T) {
	w := newApplyWorld(store.ModeAuto, TabVideos)
	w.check(nil, ok(1, true, listing("a")))
	out := w.check(nil, ok(1, false, listing("m$subscriber_only", "a")))
	if m := w.entry("fake m"); !m.Ignored || m.IgnoreReason != ReasonNeedsCookie || len(out.Auto) != 0 {
		t.Fatalf("members-only without cookies: %+v auto %s", m, ids(out.Auto))
	}
	out = w.checkWith(func(in *Input) { in.HasCookies = true }, ok(1, false, listing("m$subscriber_only", "a")))
	if m := w.entry("fake m"); m.Ignored || len(out.Auto)+len(out.Requeue) != 0 {
		t.Fatalf("with cookies: %+v, auto %s", m, ids(out.Auto))
	}
}

func TestApplyBacklogChoice(t *testing.T) {
	w := newApplyWorld(store.ModeManual, TabVideos, TabShorts)
	w.sub.Backlog = store.BacklogChoice{Mode: store.BacklogLatest, N: 3}
	// Shorts fail on the first check: the choice waits for all sources.
	out := w.check(nil, ok(1, true, listing("v5", "v4", "v3", "v2", "v1")), failed(2, ytdlp.ClassTransient, "network error"))
	if len(out.Backlog) != 0 || !w.sub.Backlog.Pending() {
		t.Fatal("backlog choice applied before all first listings succeeded")
	}
	if w.sub.State != store.SubError || w.sub.Failures != 0 || w.sub.Sources[1].Error != "network error" {
		t.Fatalf("partly failed check: %+v", w.sub)
	}
	out = w.check(nil, ok(1, false, listing("v5", "v4")), ok(2, true, listing("s2@20200101", "s1@20200101")))
	if ids(out.Backlog) != "v5,v4,v3" || w.sub.Backlog.Pending() {
		t.Fatalf("latest 3: %s", ids(out.Backlog))
	}
	if w.entry("fake s1").Backlog != true || len(out.Auto) != 0 {
		t.Fatal("a source added later records its backlog")
	}
	SortOldestFirst(out.Backlog, map[int64]*store.Source{1: w.sub.Sources[0]})
	if ids(out.Backlog) != "v3,v4,v5" {
		t.Fatalf("oldest first: %s", ids(out.Backlog))
	}
}

func TestApplyMergeAndRemovedSource(t *testing.T) {
	w := newApplyWorld(store.ModeManual, TabVideos, TabStreams)
	w.check(nil, ok(1, true, listing("a", "b")), ok(2, true, listing("b", "c")))
	if b := w.entry("fake b"); b.SourceID != 1 || b.Position != 1 || len(w.entries) != 3 {
		t.Fatalf("merge: %+v (%d entries)", b, len(w.entries))
	}
	// The streams tab was removed during the check: its results are dropped
	// and nothing is marked gone.
	w.sub.Sources = w.sub.Sources[:1]
	out := w.check(nil, ok(1, true, listing("a", "b")), ok(2, true, listing("d")))
	if w.entry("fake d") != nil || out.Summary.Gone != 1 || w.entry("fake c").GoneAt.IsZero() {
		t.Fatalf("removed source: gone %d", out.Summary.Gone)
	}
}

func TestApplyErrors(t *testing.T) {
	w := newApplyWorld(store.ModeAuto, TabVideos)
	w.check(nil, ok(1, true, listing("a")))
	w.check(nil, failed(1, ytdlp.ClassTransient, "network error"))
	if w.sub.Failures != 1 || w.sub.NextCheckAt.Sub(w.sub.LastCheckAt) != 6*time.Hour {
		t.Fatalf("first failure: %d %v", w.sub.Failures, w.sub.NextCheckAt.Sub(w.sub.LastCheckAt))
	}
	out := w.check(nil, failed(1, ytdlp.ClassBlocked, "access denied by the site"))
	if w.sub.Failures != 2 || w.sub.NextCheckAt.Sub(w.sub.LastCheckAt) != 12*time.Hour || out.EnteredError {
		t.Fatalf("backoff: %d %v", w.sub.Failures, w.sub.NextCheckAt.Sub(w.sub.LastCheckAt))
	}
	if w.sub.Hint != ytdlp.HintBlocked || w.sub.State != store.SubError {
		t.Fatalf("hint %q", w.sub.Hint)
	}
	w.check(nil, failed(1, ytdlp.ClassPermanent, "video is private"))
	if w.sub.NextCheckAt.Sub(w.sub.LastCheckAt) != 24*time.Hour {
		t.Fatal("permanent: once a day")
	}
	out = w.check(nil, ok(1, false, listing("a")))
	if w.sub.State != store.SubIdle || w.sub.Failures != 0 || w.sub.Error != "" {
		t.Fatalf("recovered: %+v", w.sub)
	}
	out = w.checkWith(func(in *Input) {
		in.Result.OptionsErr = &ytdlp.Classification{Class: ytdlp.ClassPermanent, Reason: `unknown library "Kinder"`}
	})
	if !out.EnteredError || w.sub.Error != `unknown library "Kinder"` || len(out.Insert) != 0 || w.sub.NextCheckAt.Sub(w.sub.LastCheckAt) != 24*time.Hour {
		t.Fatalf("options error: %+v", w.sub)
	}
}

// A source outside YouTube without dates: the order is guessed from the
// extractor, confirmed by where new entries appear in a complete listing,
// and entries without a date are sorted again if the guess was wrong.
func TestApplySourceOrder(t *testing.T) {
	channel := func(ids ...string) *ytdlp.Listing {
		l := listing(ids...)
		l.ExtractorKey = "ExampleUser"
		for i := range l.Entries {
			l.Entries[i].UploadDate = ""
		}
		return l
	}
	w := newApplyWorld(store.ModeManual, "")
	src := w.sub.Sources[0]
	w.check(nil, ok(1, true, channel("v3", "v2", "v1")))
	if !src.NewestFirst || src.OrderKnown || Windowed(src) {
		t.Fatalf("a user page is guessed newest first, not confirmed: %+v", src)
	}
	es := append([]*store.Entry(nil), w.entries...)
	SortNewestFirst(es, map[int64]*store.Source{1: src})
	if ids(es) != "v3,v2,v1" {
		t.Fatalf("newest first: %s", ids(es))
	}
	out := w.check(nil, ok(1, true, channel("v5", "v4", "v3", "v2", "v1")))
	if !src.NewestFirst || !src.OrderKnown || !Windowed(src) || len(out.Ordered) != 1 {
		t.Fatalf("new entries at the top confirm newest first: %+v", src)
	}

	// A playlist-like extractor is guessed oldest first; new entries at
	// the end confirm it.
	w = newApplyWorld(store.ModeManual, "")
	src = w.sub.Sources[0]
	pl := func(ids ...string) *ytdlp.Listing { l := channel(ids...); l.ExtractorKey = "ExamplePlaylist"; return l }
	w.check(nil, ok(1, true, pl("v1", "v2")))
	w.check(nil, ok(1, true, pl("v1", "v2", "v3")))
	if src.NewestFirst || !src.OrderKnown || Windowed(src) {
		t.Fatalf("playlist: %+v", src)
	}

	// A wrong guess: a user page that lists oldest first. The entries of
	// the first listing are sorted again.
	w = newApplyWorld(store.ModeManual, "")
	src = w.sub.Sources[0]
	w.check(nil, ok(1, true, channel("v1", "v2", "v3")))
	out = w.check(nil, ok(1, true, channel("v1", "v2", "v3", "v4")))
	if src.NewestFirst || !src.OrderKnown || len(out.Update) != 3 {
		t.Fatalf("new entries at the end: %+v, %d updated", src, len(out.Update))
	}
	es = append([]*store.Entry(nil), w.entries...)
	SortNewestFirst(es, map[int64]*store.Source{1: src})
	if ids(es) != "v4,v3,v2,v1" {
		t.Fatalf("sorted again: %s", ids(es))
	}

	// Dates in the listing tell the order at once.
	w = newApplyWorld(store.ModeManual, "")
	src = w.sub.Sources[0]
	w.check(nil, ok(1, true, listing("a@20240301", "b@20240201", "c@20240101")))
	if !src.NewestFirst || !src.OrderKnown {
		t.Fatalf("dated listing: %+v", src)
	}
}

func TestApplyApproximateDatesDoNotDrift(t *testing.T) {
	tab := func(at time.Time) *ytdlp.Listing {
		l := listing("v1")
		l.Entries[0].UploadDate, l.Entries[0].Timestamp = "", at.Unix()
		return l
	}
	w := newApplyWorld(store.ModeManual, "videos")
	w.check(nil, ok(1, true, tab(t0.Add(-5*time.Hour))))
	e := w.entry("fake v1")
	date, sort := e.UploadDate, e.SortDate
	if date != "20261027" {
		t.Fatalf("first date: %q", date)
	}
	// Two weeks later the tab says "2 weeks ago", a day off.
	w.check(nil, ok(1, true, tab(t0.Add(-15*24*time.Hour+14*24*time.Hour))))
	if e.UploadDate != date || !e.SortDate.Equal(sort) {
		t.Fatalf("approximate date drifted: %q %v", e.UploadDate, e.SortDate)
	}
	// A source that reports exact dates still updates it.
	w.sub.Sources = append(w.sub.Sources, &store.Source{ID: 2, SubscriptionID: 1,
		URL: "https://fake.test/pl/x", Canonical: "https://fake.test/pl/x"})
	w.check(nil, ok(2, true, listing("v1@20261026")))
	if e.UploadDate != "20261026" {
		t.Fatalf("exact date: %q", e.UploadDate)
	}
}

func TestOrderFromNew(t *testing.T) {
	for _, c := range []struct {
		pos            []int
		n              int
		newest, wantOK bool
	}{
		{[]int{0, 1}, 5, true, true},
		{[]int{3, 4}, 5, false, true},
		{[]int{0, 4}, 5, false, false},
		{nil, 5, false, false},
		{[]int{0, 1}, 2, false, false}, // everything is new
	} {
		if newest, ok := orderFromNew(c.pos, c.n); newest != c.newest || ok != c.wantOK {
			t.Errorf("%v of %d: %v %v", c.pos, c.n, newest, ok)
		}
	}
}
