package native

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/fakereddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newProvider(t *testing.T, fake *fakereddit.Server) *Provider {
	t.Helper()
	c, err := browser.New(browser.Options{RootCAs: fake.Roots, Dial: fake.Dial})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	p := New(c)
	p.PageDelay, p.MinGap = nil, 0
	return p
}

func summary(posts []reddit.Post) string {
	var out []string
	for _, p := range posts {
		out = append(out, fmt.Sprintf("%s:%s:%s", p.ID, p.Kind, p.MediaKey()))
	}
	return strings.Join(out, " ")
}

func TestListingAndPaging(t *testing.T) {
	fake := fakereddit.New(t)
	fake.Handle("www.reddit.com", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/r/EarthPorn/new/" && r.URL.Query().Get("feedViewType") == "compactView":
			w.Write(fixture(t, "listing.html"))
		case strings.HasPrefix(r.URL.Path, "/svc/shreddit/community-more-posts/new/") && r.URL.Query().Get("after") == "dDNfbG5rMQ==":
			w.Write(fixture(t, "more.html"))
		case strings.HasPrefix(r.URL.Path, "/svc/shreddit/community-more-posts/new/"):
			w.Write([]byte("")) // the end
		default:
			http.NotFound(w, r)
		}
	})
	p := newProvider(t, fake)
	tg, _ := reddit.Parse("https://www.reddit.com/r/EarthPorn/")
	pg, err := p.List(context.Background(), reddit.Query{Target: tg, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := "img1:image:redditmedia q8x2v0f3k1 gal1:gallery: vid1:video:reddit abcvid9 xp1:image:redditmedia origimg " +
		"txt1:text: nsfw1:image:redditmedia nsfwimg lnk1:link: old1:image:redditmedia oldimg"
	if got := summary(pg.Posts); got != want {
		t.Fatalf("posts:\n%s\nwant\n%s", got, want)
	}
	img := pg.Posts[0]
	if img.Title != "A lake & mountains" || img.Author != "alice" || img.Subreddit != "EarthPorn" || img.Score != 120 ||
		img.Created.Format("2006-01-02T15:04:05") != "2026-10-27T18:03:11" ||
		img.Permalink != "https://www.reddit.com/r/EarthPorn/comments/img1/a_lake/" ||
		!strings.HasPrefix(img.Thumbnail, "https://preview.redd.it/q8x2v0f3k1.jpeg") {
		t.Fatalf("image post: %+v", img)
	}
	if xp := pg.Posts[3]; xp.CrosspostParent != "orig1" || xp.Subreddit != "EarthPorn" {
		t.Fatalf("crosspost: %+v", xp)
	}
	if !pg.Posts[5].NSFW || pg.Posts[0].NSFW {
		t.Fatal("NSFW flags")
	}
	if pg.Posts[6].LinkHost() != "youtube.com" || pg.Title != "EarthPorn" || !pg.End {
		t.Fatalf("link %+v, title %q, end %v", pg.Posts[6], pg.Title, pg.End)
	}
	// The more-posts request looks like the page's own fetch.
	var more fakereddit.Request
	for _, r := range fake.Requests() {
		if strings.HasPrefix(r.Path, "/svc/") {
			more = r
			break
		}
	}
	if more.Header.Get("Sec-Fetch-Mode") != "cors" || !strings.HasPrefix(more.Header.Get("Referer"), "https://www.reddit.com/r/EarthPorn/new/") {
		t.Fatalf("more-posts headers: %v", more.Header)
	}
	// A limit stops paging.
	pg, err = p.List(context.Background(), reddit.Query{Target: tg, Limit: 3})
	if err != nil || len(pg.Posts) != 3 || pg.End {
		t.Fatalf("limited: %v %d %v", err, len(pg.Posts), pg.End)
	}
}

func TestPostPages(t *testing.T) {
	fake := fakereddit.New(t)
	fake.Handle("www.reddit.com", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/r/EarthPorn/comments/gal1/":
			w.Write(fixture(t, "post_gallery.html"))
		case "/r/example/comments/gal3/":
			w.Write(fixture(t, "post_gallery_titled.html"))
		case "/comments/vid2/":
			http.Redirect(w, r, "/r/videos/comments/vid2/clip/", http.StatusMovedPermanently)
		case "/r/videos/comments/vid2/clip/":
			w.Write(fixture(t, "post_video.html"))
		case "/r/example/comments/rem1/", "/r/example/comments/rem2/":
			w.Write(fixture(t, "post_removed.html"))
		default:
			http.NotFound(w, r)
		}
	})
	p := newProvider(t, fake)
	ctx := context.Background()
	gal, err := p.Post(ctx, "gal1", "EarthPorn")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range gal.Media {
		ids = append(ids, m.MediaID+"="+m.URL)
	}
	if gal.Kind != reddit.KindGallery || strings.Join(ids, " ") != "aaa111=https://i.redd.it/aaa111.jpg bbb222=https://i.redd.it/bbb222.png ccc333=https://i.redd.it/ccc333.jpg" ||
		gal.Media[0].Caption != "first" || !strings.HasPrefix(gal.MediaKey(), "redditgallery ") {
		t.Fatalf("gallery: %+v", gal)
	}
	// Previews named after the title: the media ids without it, each
	// image once, the originals' extensions (§5.2).
	titled, err := p.Post(ctx, "gal3", "example")
	if err != nil {
		t.Fatal(err)
	}
	ids = nil
	for _, m := range titled.Media {
		ids = append(ids, m.MediaID+"="+m.URL)
	}
	want := []reddit.Media{{MediaID: "k3m9x2abc1yh1"}, {MediaID: "p7q2w8def1yh1"}}
	if strings.Join(ids, " ") != "k3m9x2abc1yh1=https://i.redd.it/k3m9x2abc1yh1.jpeg p7q2w8def1yh1=https://i.redd.it/p7q2w8def1yh1.png" ||
		titled.MediaKey() != "redditgallery "+reddit.GalleryHash(want) {
		t.Fatalf("gallery with titled previews: %s %s", ids, titled.MediaKey())
	}
	vid, err := p.Post(ctx, "vid2", "")
	if err != nil || vid.Kind != reddit.KindVideo || vid.VideoID != "xyz789" || vid.MediaKey() != "reddit xyz789" {
		t.Fatalf("video: %v %+v", err, vid)
	}
	// A removed post: its title and banner say so, the media is gone; a
	// deleted one has only the title.
	for _, id := range []string{"rem1", "rem2"} {
		if rem, err := p.Post(ctx, id, "example"); err != nil || !rem.Removed || rem.Kind != reddit.KindText {
			t.Fatalf("removed %s: %v %+v", id, err, rem)
		}
	}
	if vid.Removed || gal.Removed {
		t.Fatal("a post counts as removed")
	}
	if _, err := p.Post(ctx, "gone1", "EarthPorn"); reddit.AsError(err).Class != ytdlp.ClassPermanent {
		t.Fatalf("missing post: %v", err)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		class  ytdlp.Class
	}{
		{200, string(fixture(t, "blocked.html")), ytdlp.ClassBlocked},
		{403, "<html>Forbidden</html>", ytdlp.ClassBlocked},
		{429, "", ytdlp.ClassRateLimited},
		{404, "<html>Sorry, nobody on Reddit goes by that name.</html>", ytdlp.ClassPermanent},
		{200, "<html>r/x has been banned from Reddit</html>", ytdlp.ClassPermanent},
		{200, "<html>This community has been quarantined</html>", ytdlp.ClassAuthRequired},
		{200, string(fixture(t, "changed.html")), ytdlp.ClassExtractorBroken},
		{200, string(fakereddit.ChallengePage("seed", "token", "/r/testsub/new/")), ytdlp.ClassBlocked},
		{502, "", ytdlp.ClassTransient},
	}
	for _, c := range cases {
		fake := fakereddit.New(t)
		fake.Handle("www.reddit.com", func(w http.ResponseWriter, r *http.Request) {
			if c.status == 429 {
				w.Header().Set("Retry-After", "120")
			}
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		})
		p := newProvider(t, fake)
		tg, _ := reddit.Parse("https://www.reddit.com/r/testsub/")
		_, err := p.List(context.Background(), reddit.Query{Target: tg, Limit: 10})
		e := reddit.AsError(err)
		if e == nil || e.Class != c.class {
			t.Errorf("%d %.40q: %+v", c.status, c.body, e)
			continue
		}
		if c.status == 429 && e.RetryAfter.Seconds() != 120 {
			t.Errorf("retry after %v", e.RetryAfter)
		}
	}
	// An empty subreddit is no error.
	fake := fakereddit.New(t)
	fake.Handle("www.reddit.com", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><shreddit-feed></shreddit-feed></html>"))
	})
	tg, _ := reddit.Parse("https://www.reddit.com/r/empty/")
	pg, err := newProvider(t, fake).List(context.Background(), reddit.Query{Target: tg, Limit: 10})
	if err != nil || len(pg.Posts) != 0 || !pg.End {
		t.Fatalf("empty: %v %+v", err, pg)
	}
	// Native can't list time ranges or anything that isn't a listing.
	if _, err := newProvider(t, fake).List(context.Background(), reddit.Query{Target: tg, After: time.Now()}); !reddit.AsError(err).Unsupported {
		t.Fatalf("time range: %v", err)
	}
}

// A JavaScript check is blocked, with its own hint, unless Solve answers
// it; the answer's URL is loaded next.
func TestChallenge(t *testing.T) {
	fake := fakereddit.New(t)
	answered := false
	fake.Handle("www.reddit.com", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("solution") == "ok" {
			answered = true
			w.Write(fixture(t, "listing.html"))
			return
		}
		w.Write(fakereddit.ChallengePage("seed", "token", r.URL.Path))
	})
	tg, _ := reddit.Parse("https://www.reddit.com/r/testsub/")
	p := newProvider(t, fake)
	_, err := p.List(context.Background(), reddit.Query{Target: tg, Limit: 3})
	if e := reddit.AsError(err); e.Class != ytdlp.ClassBlocked || e.Hint != reddit.HintChallenge {
		t.Fatalf("unanswered: %+v", e)
	}
	p.Solve = func(context.Context, []byte, *url.URL) (string, error) { return "", ErrNoAnswer }
	if _, err := p.List(context.Background(), reddit.Query{Target: tg, Limit: 3}); reddit.AsError(err).Hint != reddit.HintChallenge {
		t.Fatalf("setting off: %v", err)
	}
	p.Solve = func(_ context.Context, page []byte, u *url.URL) (string, error) {
		if !strings.Contains(string(page), "jsc_token") || u.Path != "/r/testsub/new/" {
			t.Errorf("solver got %s", u)
		}
		return "https://www.reddit.com/r/testsub/new/?solution=ok", nil
	}
	pg, err := p.List(context.Background(), reddit.Query{Target: tg, Limit: 3})
	if err != nil || !answered || len(pg.Posts) == 0 {
		t.Fatalf("answered: %v %v", err, pg)
	}
	// A failed or rejected answer is blocked (not transient), with the
	// hint for an answer that didn't work.
	p.Solve = func(context.Context, []byte, *url.URL) (string, error) { return "", errors.New("time limit") }
	_, err = p.List(context.Background(), reddit.Query{Target: tg, Limit: 3})
	if e := reddit.AsError(err); e.Class != ytdlp.ClassBlocked || e.Hint != reddit.HintChallengeFailed ||
		!strings.Contains(e.Reason, "could not be answered: time limit") {
		t.Fatalf("failed answer: %+v", e)
	}
	p.Solve = func(context.Context, []byte, *url.URL) (string, error) {
		return "https://www.reddit.com/r/testsub/new/?solution=wrong", nil
	}
	_, err = p.List(context.Background(), reddit.Query{Target: tg, Limit: 3})
	if e := reddit.AsError(err); e.Class != ytdlp.ClassBlocked || e.Hint != reddit.HintChallengeFailed ||
		!strings.Contains(e.Reason, "did not accept") {
		t.Fatalf("rejected answer: %+v", e)
	}
}

// Reddit answers a post URL without its title, and a subreddit in another
// spelling, with a page whose script only redirects to the canonical URL.
// Native follows it once, and asks for the subreddit in its own spelling
// from then on.
func TestScriptRedirects(t *testing.T) {
	site := fakereddit.NewSite(t)
	now := time.Now().Truncate(time.Second)
	site.Add(&fakereddit.Post{ID: "abc12", Subreddit: "Example", Title: "A title", Kind: "image", Media: []string{"img1"},
		Created: now, Slug: "a_title"},
		&fakereddit.Post{ID: "abc13", Subreddit: "Example", Title: "Older", Kind: "image", Media: []string{"img2"},
			Created: now.Add(-time.Minute)})
	site.PageSize = 1
	p := newProvider(t, site.Server)
	ctx := context.Background()

	post, err := p.Post(ctx, "abc12", "Example")
	if err != nil || post.ID != "abc12" || post.MediaKey() != "redditmedia img1" {
		t.Fatalf("post: %+v %v", post, err)
	}
	if n := site.Count("www.reddit.com", "/r/Example/comments/abc12/a_title/"); n != 1 {
		t.Fatalf("%d requests of the canonical post URL", n)
	}

	tg, _ := reddit.Parse("https://www.reddit.com/r/example/")
	for i := 0; i < 2; i++ {
		pg, err := p.List(ctx, reddit.Query{Target: tg, Limit: 10})
		if err != nil || summary(pg.Posts) != "abc12:image:redditmedia img1 abc13:image:redditmedia img2" {
			t.Fatalf("listing %d: %v %v", i, pg, err)
		}
		if pg.Icon != "https://styles.redditmedia.com/t5_1/styles/communityIcon_Example.png" {
			t.Fatalf("icon %q", pg.Icon)
		}
	}
	if n := site.Count("www.reddit.com", "/r/example/new"); n != 1 {
		t.Fatalf("%d requests in the other spelling, want 1 (then the learned one)", n)
	}
	var more []string
	for _, r := range site.Requests() {
		if strings.HasPrefix(r.Path, "/svc/shreddit/community-more-posts/") {
			q, _ := url.ParseQuery(r.Query)
			more = append(more, q.Get("name"))
		}
	}
	if len(more) < 2 || strings.Trim(strings.Join(more, ","), "Example,") != "" {
		t.Fatalf("more posts asked for %v", more)
	}
}

// A redirect is only followed on Reddit's own host.
func TestScriptRedirectToOtherHost(t *testing.T) {
	fake := fakereddit.New(t)
	fake.Handle("www.reddit.com", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fakereddit.ShellPage("https://i.redd.it/elsewhere"))
	})
	p := newProvider(t, fake)
	_, err := p.Post(context.Background(), "abc12", "example")
	if e := reddit.AsError(err); e.Class != ytdlp.ClassExtractorBroken || len(fake.Requests()) != 1 {
		t.Fatalf("%v after %d requests", err, len(fake.Requests()))
	}
}

// Every community page says "quarantined":false; only the page data's
// flag marks a quarantined community.
func TestQuarantine(t *testing.T) {
	site := fakereddit.NewSite(t)
	p := newProvider(t, site.Server)
	ctx := context.Background()
	tg, _ := reddit.Parse("https://www.reddit.com/r/empty/")
	pg, err := p.List(ctx, reddit.Query{Target: tg, Limit: 10})
	if err != nil || len(pg.Posts) != 0 || !pg.End {
		t.Fatalf("empty subreddit: %+v %v", pg, err)
	}
	site.SetQuarantined("gated")
	tg, _ = reddit.Parse("https://www.reddit.com/r/gated/")
	if _, err := p.List(ctx, reddit.Query{Target: tg, Limit: 10}); reddit.AsError(err).Class != ytdlp.ClassAuthRequired {
		t.Fatalf("quarantined: %v", err)
	}
}

// Through a chain, native notes its steps in the chain's log: the pages
// of a listing, a script redirect and an answered check. Page loads keep
// MinGap apart, also across requests.
func TestStepsAndGap(t *testing.T) {
	fake := fakereddit.New(t)
	fake.Handle("www.reddit.com", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Query().Get("solution") == "ok":
			w.Write(fixture(t, "post_video.html"))
		case r.URL.Path == "/r/videos/comments/vid2/":
			w.Write(fakereddit.ShellPage("/r/videos/comments/vid2/clip/"))
		case r.URL.Path == "/r/videos/comments/vid2/clip/":
			w.Write(fakereddit.ChallengePage("seed", "token", r.URL.Path))
		case r.URL.Path == "/r/EarthPorn/new/":
			w.Write(fixture(t, "listing.html"))
		case strings.HasPrefix(r.URL.Path, "/svc/shreddit/community-more-posts/new/") && r.URL.Query().Get("after") == "dDNfbG5rMQ==":
			w.Write(fixture(t, "more.html"))
		case strings.HasPrefix(r.URL.Path, "/svc/shreddit/"):
			w.Write([]byte(""))
		default:
			http.NotFound(w, r)
		}
	})
	p := newProvider(t, fake)
	p.Solve = func(_ context.Context, _ []byte, u *url.URL) (string, error) {
		return u.String() + "?solution=ok", nil
	}
	p.MinGap = 40 * time.Millisecond
	var lines []string
	chain := &reddit.Chain{Providers: []reddit.Provider{p}, Logf: func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }}
	start := time.Now()
	if _, _, err := chain.Post(context.Background(), "vid2", "videos"); err != nil {
		t.Fatal(err)
	}
	tg, _ := reddit.Parse("https://www.reddit.com/r/EarthPorn/")
	if _, _, err := chain.List(context.Background(), reddit.Query{Target: tg, Limit: 100}); err != nil {
		t.Fatal(err)
	}
	log := strings.Join(lines, "\n")
	for _, want := range []string{"native: /r/videos/comments/vid2/ redirects to /r/videos/comments/vid2/clip/",
		"native: answered Reddit's JavaScript check", "native: page 1, ", "native: page 2, "} {
		if !strings.Contains(log, want) {
			t.Errorf("%q missing:\n%s", want, log)
		}
	}
	// 2 loads of the post (the answer to the check is part of the
	// second), 3 of the listing: 4 gaps.
	if d := time.Since(start); d < 4*p.MinGap {
		t.Fatalf("5 page loads in %v", d)
	}
}
