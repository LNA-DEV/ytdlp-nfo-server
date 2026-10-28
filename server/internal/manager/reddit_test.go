package manager

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
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

	// Both blocked: a classified error with the providers tried.
	site.SetArcticDown(true)
	sub = w.checkNow(id)
	if sub.ErrorClass != "blocked" || !strings.Contains(sub.Hint, "Tried: native, arcticshift") {
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
