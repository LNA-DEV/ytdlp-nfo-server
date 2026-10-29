package manager

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/fakereddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/scenario"
)

// redditWorld is a world with a fake Reddit.
func redditWorld(t *testing.T) (*world, *fakereddit.Site) {
	t.Helper()
	site := fakereddit.NewSite(t)
	w := newWorld(t, &scenario.Scenario{})
	w.reddit = &browser.Options{RootCAs: site.Roots, Dial: site.Dial}
	w.startSubs()
	return w, site
}

// mixedPosts returns n posts of r/<sub>, newest first, one per minute
// before base: images, videos, galleries, text and link posts.
func mixedPosts(sub string, base time.Time, from, n int) []*fakereddit.Post {
	var out []*fakereddit.Post
	for i := from; i < from+n; i++ {
		p := &fakereddit.Post{ID: fmt.Sprintf("p%d", i), Subreddit: sub, Author: "u", Title: fmt.Sprintf("Post %d", i),
			Created: base.Add(-time.Duration(i) * time.Minute)}
		switch i % 5 {
		case 0:
			p.Kind, p.Media = "image", []string{fmt.Sprintf("img%d", i)}
		case 1:
			p.Kind, p.Media = "video", []string{fmt.Sprintf("vid%d", i)}
		case 2:
			p.Kind, p.Media = "gallery", []string{fmt.Sprintf("g%da", i), fmt.Sprintf("g%db", i)}
		case 3:
			p.Kind = "text"
		case 4:
			p.Kind, p.Link = "link", "https://www.youtube.com/watch?v=x"+fmt.Sprint(i)
		}
		out = append(out, p)
	}
	return out
}

func keysOf(es []EntryView) string {
	var out []string
	for _, e := range es {
		out = append(out, e.Key)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestRedditSubscription(t *testing.T) {
	w, site := redditWorld(t)
	now := time.Now().Truncate(time.Second)
	site.Add(mixedPosts("EarthPorn", now, 0, 10)...)
	ctx := context.Background()

	// The preview of a post switches to its subreddit.
	p, err := w.m.PreviewSubscription(ctx, "https://www.reddit.com/r/EarthPorn/comments/p0/post/")
	if err != nil {
		t.Fatal(err)
	}
	if p.Site != "reddit" || p.Name != "r/EarthPorn" || p.Folder != "EarthPorn" || p.ResolvedFrom != "Post 0" ||
		len(p.Sources) != 1 || p.Sources[0].Canonical != "https://www.reddit.com/r/earthporn/new" || len(p.Entries) != 10 {
		t.Fatalf("preview: %+v", p)
	}

	created := time.Now()
	v := w.subscribe(SubscriptionInput{Sources: []SourceInput{{URL: "https://old.reddit.com/r/EarthPorn/"}}})
	if v.Name != "r/EarthPorn" || v.Folder != "EarthPorn" || !v.Sources[0].NewestFirst || !v.Sources[0].OrderKnown {
		t.Fatalf("subscription: %+v", v)
	}
	id := sid(v)
	sub := w.waitChecked(id, created)
	if sub.Error != "" {
		t.Fatalf("check error: %s %s", sub.Error, sub.Hint)
	}
	backlog := w.entries(id, "backlog")
	// 2 images, 2 videos (media keys), 2 galleries (post keys: native
	// listings don't show gallery media); text and links are ignored.
	if keysOf(backlog) != "reddit vid1,reddit vid6,redditmedia img0,redditmedia img5,redditpost p2,redditpost p7" {
		t.Fatalf("backlog: %s", keysOf(backlog))
	}
	ignored := w.entries(id, "ignored")
	reasons := map[string]string{}
	for _, e := range ignored {
		reasons[e.Key] = e.IgnoreReason
	}
	if len(ignored) != 4 || reasons["redditpost p3"] != "no media" || reasons["redditpost p4"] != "link to youtube.com" {
		t.Fatalf("ignored: %v", reasons)
	}
	for _, e := range backlog {
		if e.Key == "redditmedia img0" && (e.MediaKind != "image" || e.Title != "Post 0" || e.URL != "https://www.reddit.com/r/EarthPorn/comments/p0/post/") {
			t.Fatalf("entry: %+v", e)
		}
	}
	log, _ := w.m.subs.logs.Read(id)
	if !strings.Contains(string(log), "via native") || strings.Contains(string(log), "yt-dlp ") {
		t.Fatalf("check log:\n%s", log)
	}

	// New posts appear; the filter keeps images only.
	f := store.Filters{MediaKinds: []string{"image"}}
	if _, err := w.m.UpdateSubscription(ctx, id, SubscriptionInput{Filters: &f}); err != nil {
		t.Fatal(err)
	}
	site.Add(mixedPosts("EarthPorn", now.Add(time.Hour), 100, 5)...) // p100 image … p104 link
	w.checkNow(id)
	newEntries := w.entries(id, "new")
	if keysOf(newEntries) != "redditmedia img100" {
		t.Fatalf("new: %s", keysOf(newEntries))
	}
	var filtered int
	for _, e := range w.entries(id, "ignored") {
		if strings.HasPrefix(e.IgnoreReason, "filter: ") {
			filtered++
		}
	}
	if filtered < 2 { // the new video and gallery (and the old ones, re-evaluated)
		t.Fatalf("filtered: %d", filtered)
	}

	// Native blocked: Arctic Shift answers, with the same keys.
	site.SetNativeBlocked(true)
	site.Add(mixedPosts("EarthPorn", now.Add(2*time.Hour), 200, 1)...) // p200 image
	sub = w.checkNow(id)
	if sub.Error != "" {
		t.Fatalf("fallback failed: %s", sub.Error)
	}
	log, _ = w.m.subs.logs.Read(id)
	if !strings.Contains(string(log), "native: failed (blocked") || !strings.Contains(string(log), "via arcticshift") {
		t.Fatalf("log:\n%s", log)
	}
	if keysOf(w.entries(id, "new")) != "redditmedia img100,redditmedia img200" {
		t.Fatalf("new after fallback: %s", keysOf(w.entries(id, "new")))
	}
	total := len(w.entries(id, ""))
	if total != 16 {
		t.Fatalf("entries: %d (duplicates?)", total)
	}

	// Both failing: native's error with the providers tried, as temporary
	// because Arctic Shift's error is.
	site.SetArcticDown(true)
	sub = w.checkNow(id)
	if sub.ErrorClass != "transient" || !strings.Contains(sub.Error, "blocked by Reddit's network security (arcticshift: ") ||
		!strings.HasPrefix(sub.Hint, reddit.HintBlocked) || !strings.Contains(sub.Hint, "Tried: native, arcticshift") {
		t.Fatalf("both failing: %s %s %s", sub.ErrorClass, sub.Error, sub.Hint)
	}
}

// More than native can list appeared since the last check: the gap is
// filled from Arctic Shift by time, and the subscription is warned.
func TestRedditGap(t *testing.T) {
	w, site := redditWorld(t)
	ctx := context.Background()
	site.PageSize = 100
	start := time.Now().Truncate(time.Second)
	site.Add(mixedPosts("big", start, 2000, 5)...)
	created := time.Now()
	v := w.subscribe(SubscriptionInput{Sources: []SourceInput{{URL: "https://www.reddit.com/r/big/"}}})
	id := sid(v)
	w.waitChecked(id, created)
	// 1100 new posts, all images, created after the first check.
	var posts []*fakereddit.Post
	for i := 0; i < 1100; i++ {
		posts = append(posts, &fakereddit.Post{ID: fmt.Sprintf("n%d", i), Subreddit: "big", Title: "n", Kind: "image",
			Media: []string{fmt.Sprintf("ni%d", i)}, Created: start.Add(time.Hour - time.Duration(i)*time.Second)})
	}
	site.Add(posts...)
	sub := w.checkNow(id)
	if n := w.newCount(id); n != 1100 {
		log, _ := w.m.subs.logs.Read(id)
		t.Fatalf("new entries: %d\n%s", n, log)
	}
	if !strings.Contains(sub.Warning, "More than 1000 new posts") || !strings.Contains(sub.Warning, "filled from Arctic Shift") {
		t.Fatalf("warning: %q", sub.Warning)
	}
	// Without Arctic Shift the gap is only reported.
	if _, err := w.m.Settings.Update(ctx, func(s *config.Settings) error { s.Reddit.Providers = []string{"native"}; return nil }); err != nil {
		t.Fatal(err)
	}
	posts = nil
	for i := 0; i < 1050; i++ {
		posts = append(posts, &fakereddit.Post{ID: fmt.Sprintf("m%d", i), Subreddit: "big", Title: "m", Kind: "image",
			Media: []string{fmt.Sprintf("mi%d", i)}, Created: start.Add(2*time.Hour - time.Duration(i)*time.Second)})
	}
	site.Add(posts...)
	sub = w.checkNow(id)
	if !strings.Contains(sub.Warning, "may be missing") || !strings.Contains(sub.Warning, "Check at least every") {
		t.Fatalf("warning without Arctic Shift: %q", sub.Warning)
	}
	// A check without a gap clears the warning.
	sub = w.checkNow(id)
	if sub.Warning != "" {
		t.Fatalf("warning kept: %q", sub.Warning)
	}
}

func (w *world) newCount(id int64) int {
	w.t.Helper()
	c, err := w.m.Store.CountEntries(context.Background(), id, w.m.statusParams())
	if err != nil {
		w.t.Fatal(err)
	}
	return c[id].New
}

// Feed readers export Reddit feeds as OPML; Reddit ends RSS on 2026-11-13
// (Reddit concept §11).
func TestRedditRSSImport(t *testing.T) {
	w, _ := redditWorld(t)
	opml := `<?xml version="1.0"?><opml version="2.0"><body><outline text="Reddit">
<outline type="rss" text="EarthPorn" xmlUrl="https://www.reddit.com/r/EarthPorn/.rss" htmlUrl="https://www.reddit.com/r/EarthPorn"/>
<outline type="rss" text="https://www.reddit.com/r/pics/new/.rss" xmlUrl="https://www.reddit.com/r/pics/new/.rss"/>
<outline type="rss" text="dup" xmlUrl="https://old.reddit.com/r/earthporn/new/.rss"/>
</outline></body></opml>`
	res, err := w.m.ImportSubscriptions(context.Background(), FormatOPML, opml)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range res.Created {
		got = append(got, v.Name+"|"+v.Folder+"|"+v.Sources[0].Canonical)
	}
	sort.Strings(got)
	want := "EarthPorn|EarthPorn|https://www.reddit.com/r/earthporn/new r/pics|pics|https://www.reddit.com/r/pics/new"
	if strings.Join(got, " ") != want || len(res.Skipped) != 1 || res.Skipped[0].Reason != "listed twice" {
		t.Fatalf("import: %v skipped %+v", got, res.Skipped)
	}
}

// The first check of a subreddit with more posts than firstListing lists
// once and ends (it used to repeat the listing without end).
func TestRedditFirstCheckListsOnce(t *testing.T) {
	w, site := redditWorld(t)
	site.Add(mixedPosts("example", time.Now().Truncate(time.Second), 0, 300)...)
	created := time.Now()
	v := w.subscribe(SubscriptionInput{Sources: []SourceInput{{URL: "https://www.reddit.com/r/example/"}}})
	id := sid(v)
	sub := w.waitChecked(id, created)
	if n := site.Count("www.reddit.com", "/r/example/new"); n != 1 || sub.Error != "" {
		log, _ := w.m.subs.logs.Read(id)
		t.Fatalf("%d listings, error %q\n%s", n, sub.Error, log)
	}
	if n := len(w.entries(id, "")); n != 100 {
		t.Fatalf("%d entries, want firstListing = 100", n)
	}
}

// Reddit answers native with a JavaScript check. With the setting off it
// counts as blocked: Arctic Shift answers, and native is paused, so the
// posts of a job don't ask native again; resetting the Reddit session ends
// the pause. With the setting on (the default) the check is answered once
// and its cookie kept.
func TestRedditChallenge(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	nativeRequests := func(site *fakereddit.Site) int {
		return site.Count("www.reddit.com", "/")
	}

	w, site := redditMediaWorld(t, "vid1", "vid6")
	site.Add(mixedPosts("example", now, 0, 10)...)
	site.SetChallenge(true)
	if _, err := w.m.Settings.Update(ctx, func(s *config.Settings) error { s.Reddit.SolveChallenge = false; return nil }); err != nil {
		t.Fatal(err)
	}
	created := time.Now()
	v := w.subscribe(SubscriptionInput{Sources: []SourceInput{{URL: "https://www.reddit.com/r/example/"}}})
	id := sid(v)
	sub := w.waitChecked(id, created)
	log, _ := w.m.subs.logs.Read(id)
	if sub.Error != "" || !strings.Contains(string(log), "native: failed (blocked: Reddit asks for a JavaScript check)") ||
		!strings.Contains(string(log), "via arcticshift") {
		t.Fatalf("check: %q\n%s", sub.Error, log)
	}
	if until := w.m.Sched.Status().Cooldowns["reddit-native"]; until.Before(time.Now().Add(20 * time.Minute)) {
		t.Fatalf("native not paused: %v", until)
	}
	if w.m.Sched.Status().Cooldowns["reddit.com"].After(time.Now()) {
		t.Fatal("Reddit jobs paused with native")
	}
	before := nativeRequests(site)
	if _, err := w.m.DownloadEntries(ctx, DownloadEntriesRequest{Subscription: fmt.Sprint(id), Status: "backlog"}); err != nil {
		t.Fatal(err)
	}
	for _, j := range w.subJobs(id) {
		if j = w.wait(j.ID, store.JobCompleted, store.JobPartial, store.JobFailed); j.State != store.JobCompleted {
			t.Fatalf("job: %s\n%s", j.State, w.log(j.ID))
		}
	}
	if n := nativeRequests(site) - before; n != 0 {
		t.Fatalf("%d requests to the paused native", n)
	}

	// Turned on, the setting applies once the session is reset: native is
	// asked again at once and answers the check.
	if _, err := w.m.Settings.Update(ctx, func(s *config.Settings) error { s.Reddit.SolveChallenge = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := w.m.ResetReddit(); err != nil {
		t.Fatal(err)
	}
	if w.m.Sched.Status().Cooldowns["reddit-native"].After(time.Now()) {
		t.Fatal("native still paused after the reset")
	}
	site.Add(mixedPosts("example", now.Add(time.Hour), 10, 1)...)
	w.checkNow(id)
	if log, _ := w.m.subs.logs.Read(id); site.Solved != 1 || !strings.Contains(string(log), "Listed 11 entries via native") {
		t.Fatalf("after the reset: %d solved\n%s", site.Solved, log)
	}

	// By default, a new server answers the check and keeps the cookie.
	w2, site2 := redditWorld(t)
	site2.Add(mixedPosts("example", now, 0, 10)...)
	site2.SetChallenge(true)
	created = time.Now()
	v = w2.subscribe(SubscriptionInput{Sources: []SourceInput{{URL: "https://www.reddit.com/r/example/"}}})
	id = sid(v)
	sub = w2.waitChecked(id, created)
	log, _ = w2.m.subs.logs.Read(id)
	if sub.Error != "" || !strings.Contains(string(log), "via native") || site2.Solved != 1 {
		t.Fatalf("answered check: %q, %d solved\n%s", sub.Error, site2.Solved, log)
	}
	w2.checkNow(id)
	if site2.Solved != 1 {
		t.Fatalf("the cookie was not kept: %d checks answered", site2.Solved)
	}
}

// A top list as a source (Reddit concept §7.7): every check lists exactly
// the top N, ranked by score; a post that enters the top N is a new entry.
// Only native ranks, so the check fails without it.
func TestRedditTopSource(t *testing.T) {
	w, site := redditWorld(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	for i := 0; i < 8; i++ {
		site.Add(&fakereddit.Post{ID: fmt.Sprintf("t%d", i), Subreddit: "example", Title: fmt.Sprintf("Top %d", i), Kind: "image",
			Media: []string{fmt.Sprintf("ti%d", i)}, Created: now.Add(-time.Duration(i) * time.Hour), Score: (8 - i) * 10})
	}
	topURL := "https://www.reddit.com/r/example/top/?t=week"
	p, err := w.m.PreviewSubscription(ctx, topURL)
	if err != nil || len(p.Sources) != 1 || p.Sources[0].ListLimit != 10 ||
		p.Sources[0].Canonical != "https://www.reddit.com/r/example/top?t=week" || p.Entries[0].Key != "redditmedia ti0" {
		t.Fatalf("preview: %+v %v", p, err)
	}
	if _, err := w.m.CreateSubscription(ctx, SubscriptionInput{Sources: []SourceInput{{URL: "https://www.reddit.com/r/example/hot/"}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("hot list subscribed: %v", err)
	}
	if _, err := w.m.CreateSubscription(ctx, SubscriptionInput{Sources: []SourceInput{{URL: "https://www.reddit.com/r/example/", ListLimit: 5}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("N for a listing of new posts: %v", err)
	}

	created := time.Now()
	v := w.subscribe(SubscriptionInput{Sources: []SourceInput{{URL: topURL, ListLimit: 3}}})
	id := sid(v)
	w.waitChecked(id, created)
	// The newest posts rank highest here, so the dates of the list look
	// newest first; its order is still not learned from them.
	if got := keysOf(w.entries(id, "backlog")); got != "redditmedia ti0,redditmedia ti1,redditmedia ti2" {
		t.Fatalf("backlog: %s", got)
	}
	sv, _ := w.m.GetSubscription(ctx, id)
	if src := sv.Sources[0]; src.ListLimit != 3 || src.NewestFirst || !src.OrderKnown {
		t.Fatalf("source: %+v", src)
	}

	// An older post enters the top 3, a newer one doesn't: only the first
	// is new, and nothing counts as a gap.
	site.Add(&fakereddit.Post{ID: "up", Subreddit: "example", Title: "Rising", Kind: "image", Media: []string{"tiup"},
		Created: now.Add(-48 * time.Hour), Score: 65},
		&fakereddit.Post{ID: "low", Subreddit: "example", Title: "Low", Kind: "image", Media: []string{"tilow"}, Created: now, Score: 1})
	sub := w.checkNow(id)
	log, _ := w.m.subs.logs.Read(id)
	if got := keysOf(w.entries(id, "new")); got != "redditmedia tiup" || sub.Warning != "" ||
		!strings.Contains(string(log), "(top 3)") || strings.Contains(string(log), "listing more") {
		t.Fatalf("new %s, warning %q\n%s", got, sub.Warning, log)
	}
	sv, _ = w.m.GetSubscription(ctx, id)
	if src := sv.Sources[0]; src.NewestFirst || !src.OrderKnown {
		t.Fatalf("order learned from the dates of a top list: %+v", src)
	}

	// N can change.
	if _, err := w.m.UpdateSubscription(ctx, id, SubscriptionInput{ListLimits: map[string]int{sv.Sources[0].ID: 5}}); err != nil {
		t.Fatal(err)
	}
	if sv, _ = w.m.GetSubscription(ctx, id); sv.Sources[0].ListLimit != 5 {
		t.Fatalf("N: %d", sv.Sources[0].ListLimit)
	}

	// Native blocked: no fall-through to Arctic Shift, which can't rank.
	site.SetNativeBlocked(true)
	before := site.Count("arctic-shift.photon-reddit.com", "/api/posts/search")
	sub = w.checkNow(id)
	if !strings.Contains(sub.Hint, reddit.HintTopNative) || sub.ErrorClass == "" ||
		site.Count("arctic-shift.photon-reddit.com", "/api/posts/search") != before {
		t.Fatalf("blocked: %s %q %q", sub.ErrorClass, sub.Error, sub.Hint)
	}
}

// backfillPosts adds n posts of r/example, one every two days back from
// base: image, gallery, video and text in turn, scored by score(i).
func backfillPosts(site *fakereddit.Site, base time.Time, n int, score func(int) int) {
	kinds := []string{"image", "gallery", "video", "text"}
	for i := 0; i < n; i++ {
		p := &fakereddit.Post{ID: fmt.Sprintf("b%d", i), Subreddit: "example", Author: "u", Title: fmt.Sprintf("Old %d", i),
			Kind: kinds[i%4], Created: base.Add(-time.Duration(i) * 48 * time.Hour), Score: score(i)}
		switch p.Kind {
		case "image", "video":
			p.Media = []string{fmt.Sprintf("bm%d", i)}
		case "gallery":
			p.Media = []string{fmt.Sprintf("bg%da", i), fmt.Sprintf("bg%db", i)}
		}
		site.Add(p)
	}
}

// waitBackfill waits until the subscription's backfill request ran.
func (w *world) waitBackfill(id int64) *SubscriptionView {
	w.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		v, err := w.m.GetSubscription(context.Background(), id)
		if err == nil && (v.Backfill == nil || !v.Backfill.Pending) && v.State != store.SubChecking {
			return v
		}
		if time.Now().After(deadline) {
			log, _ := w.m.subs.logs.Read(id)
			w.t.Fatalf("backfill did not run: %+v\n%s", v.Backfill, log)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Loading older posts of a time range from Arctic Shift (Reddit concept
// §7.7): counted first, at most backfillMax per run, Continue resumes
// before the oldest post loaded, and everything ends up in the backlog
// once, without text posts when the media kinds say so.
func TestRedditBackfillRange(t *testing.T) {
	w, site := redditWorld(t)
	ctx := context.Background()
	if _, err := w.m.Settings.Update(ctx, func(s *config.Settings) error {
		s.Reddit.FirstListing, s.Reddit.BackfillMax = 25, 100
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Truncate(time.Second)
	backfillPosts(site, base, 260, func(i int) int { return i })
	created := time.Now()
	v := w.subscribe(SubscriptionInput{Sources: []SourceInput{{URL: "https://www.reddit.com/r/example/"}},
		Filters: &store.Filters{MediaKinds: []string{"image", "gallery"}}})
	id := sid(v)
	w.waitChecked(id, created)
	if n := len(w.entries(id, "")); n != 25 {
		t.Fatalf("first listing: %d entries", n)
	}

	// The count uses the filters: images and galleries, half of the posts.
	c, err := w.m.PreviewBackfill(ctx, id, "", "")
	sum := 0
	for _, y := range c.Years {
		sum += y.Count
	}
	if err != nil || !c.Known || c.Total != 130 || sum != 130 || len(c.Years) < 2 || c.BackfillMax != 100 {
		t.Fatalf("counts: %+v %v", c, err)
	}
	if _, err := w.m.PreviewBackfill(ctx, id, "2026-05-01", "2026-04-01"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reversed range: %v", err)
	}

	if _, err := w.m.RequestBackfill(ctx, id, BackfillInput{Kind: store.BackfillRange}); err != nil {
		t.Fatal(err)
	}
	v = w.waitBackfill(id)
	if b := v.Backfill; b == nil || !b.CanContinue || b.Loaded != 100 || b.Total != 130 {
		t.Fatalf("first run: %+v", b)
	}
	for _, q := range site.ArcticSearches() {
		if q.Get("url") != "https://i.redd.it/" && q.Get("url") != "https://www.reddit.com/gallery/" {
			t.Fatalf("search without a media prefix: %v", q)
		}
	}
	for i := 0; i < 3 && v.Backfill != nil; i++ {
		if _, err := w.m.RequestBackfill(ctx, id, BackfillInput{Continue: true}); err != nil {
			t.Fatal(err)
		}
		v = w.waitBackfill(id)
	}
	if v.Backfill != nil {
		t.Fatalf("not done: %+v", v.Backfill)
	}
	// 25 from the first listing (with text and video posts) and the other
	// 130 - (images and galleries among them) older ones; no text post
	// was transferred, nothing twice.
	es := w.entries(id, "")
	keys := map[string]bool{}
	for _, e := range es {
		if keys[e.Key] {
			t.Fatalf("%s twice", e.Key)
		}
		keys[e.Key] = true
	}
	if len(es) != 25+130-13 || len(w.entries(id, "backlog")) != 130 || len(w.entries(id, "new")) != 0 {
		t.Fatalf("%d entries, %d backlog", len(es), len(w.entries(id, "backlog")))
	}
	if _, err := w.m.RequestBackfill(ctx, id, BackfillInput{Continue: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("continue without a request: %v", err)
	}

	// A count that times out leaves the total unknown; the request stays
	// until it runs, and can be cancelled.
	site.SetAggregateFails(true)
	if c, _ := w.m.PreviewBackfill(ctx, id, "", ""); c.Known {
		t.Fatalf("counts after a timeout: %+v", c)
	}
	off := false
	if _, err := w.m.UpdateSubscription(ctx, id, SubscriptionInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	v, err = w.m.RequestBackfill(ctx, id, BackfillInput{Kind: store.BackfillRange, From: "2025-01-01", To: "2025-12-31"})
	if err != nil || v.Backfill == nil || !v.Backfill.Pending || v.Backfill.Total != 0 || v.Backfill.From != "2025-01-01" ||
		v.Backfill.To != "2025-12-31" {
		t.Fatalf("request: %+v %v", v.Backfill, err)
	}
	if v, err = w.m.CancelBackfill(ctx, id); err != nil || v.Backfill != nil {
		t.Fatalf("cancel: %+v %v", v, err)
	}
}
