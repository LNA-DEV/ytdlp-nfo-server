package arcticshift

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/fakereddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

func newProvider(t *testing.T, fake *fakereddit.Server) *Provider {
	t.Helper()
	c, err := browser.New(browser.Options{RootCAs: fake.Roots, Dial: fake.Dial})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	p := New(c)
	p.MinInterval = 0
	return p
}

func TestConvert(t *testing.T) {
	data, err := os.ReadFile("testdata/posts.json")
	if err != nil {
		t.Fatal(err)
	}
	fake := fakereddit.New(t)
	fake.Handle("arctic-shift.photon-reddit.com", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/posts/search" || r.URL.Query().Get("subreddit") != "EarthPorn" || r.URL.Query().Get("sort") != "desc" {
			http.Error(w, `{"error":"bad request"}`, 400)
			return
		}
		w.Write(data)
	})
	p := newProvider(t, fake)
	tg, _ := reddit.Parse("https://www.reddit.com/r/EarthPorn/")
	pg, err := p.List(context.Background(), reddit.Query{Target: tg, Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ps := range pg.Posts {
		got = append(got, fmt.Sprintf("%s:%s:%s", ps.ID, ps.Kind, ps.MediaKey()))
	}
	want := "img1:image:redditmedia q8x2v0f3k1 gal1:gallery:" + reddit.GalleryHash([]reddit.Media{{MediaID: "aaa111"}, {MediaID: "bbb222"}, {MediaID: "anim3"}}) +
		" vid1:video:reddit abcvid9 xp1:image:redditmedia origimg txt1:text: nsfw1:image:redditmedia nsfwimg lnk1:link: del1:image:redditmedia goneimg"
	want = strings.Replace(want, "gal1:gallery:", "gal1:gallery:redditgallery ", 1)
	if strings.Join(got, " ") != want {
		t.Fatalf("posts:\n%s\nwant\n%s", strings.Join(got, " "), want)
	}
	img, gal, vid, xp := pg.Posts[0], pg.Posts[1], pg.Posts[2], pg.Posts[3]
	if img.Title != "A lake & mountains" || img.Created.Unix() != 1761588191 || img.Permalink != "https://www.reddit.com/r/EarthPorn/comments/img1/a_lake/" ||
		img.Thumbnail != "https://preview.redd.it/q8x2v0f3k1.jpeg?width=640&s=c" {
		t.Fatalf("image: %+v", img)
	}
	if len(gal.Media) != 3 || gal.Media[0].URL != "https://i.redd.it/aaa111.jpg" || gal.Media[0].Caption != "first" ||
		gal.Media[1].URL != "https://i.redd.it/bbb222.png" || gal.Media[2].URL != "https://i.redd.it/anim3.gif" || gal.Created.Unix() != 1761584400 {
		t.Fatalf("gallery: %+v", gal)
	}
	if vid.Duration != 42 || xp.CrosspostParent != "orig1" || xp.Subreddit != "EarthPorn" {
		t.Fatalf("video %+v / crosspost %+v", vid, xp)
	}
	if !pg.Posts[5].NSFW || !pg.Posts[7].Removed || pg.Posts[6].LinkHost() != "youtube.com" {
		t.Fatal("flags")
	}
}

// Listings page by time, newest first, and stop at the limit or the end.
func TestPagingAndErrors(t *testing.T) {
	fake := fakereddit.New(t)
	const newest = int64(1761588000)
	var calls int
	fake.Handle("arctic-shift.photon-reddit.com", func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		if q.Get("author") == "limited" {
			w.Header().Set("X-RateLimit-Reset", "30")
			w.WriteHeader(429)
			return
		}
		if q.Get("subreddit") == "slow" {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"Query timed out"}`))
			return
		}
		before := newest + 1
		if b := q.Get("before"); b != "" {
			before, _ = strconv.ParseInt(b, 10, 64)
		}
		after := newest - 250*60
		if a := q.Get("after"); a != "" {
			after, _ = strconv.ParseInt(a, 10, 64)
		}
		n, _ := strconv.Atoi(q.Get("limit"))
		var data []map[string]any
		// 250 posts, one per minute; the oldest is newest-249 min.
		for ts := before - 1; len(data) < n && ts > after && ts > newest-250*60; ts-- {
			if (newest-ts)%60 == 0 {
				data = append(data, map[string]any{"id": fmt.Sprintf("p%d", (newest-ts)/60), "subreddit": "big", "created_utc": ts,
					"is_self": true, "permalink": "/r/big/comments/x/"})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	})
	p := newProvider(t, fake)
	ctx := context.Background()
	tg, _ := reddit.Parse("https://www.reddit.com/r/big/")
	pg, err := p.List(ctx, reddit.Query{Target: tg, Limit: 150})
	if err != nil || len(pg.Posts) != 150 || pg.Posts[0].ID != "p0" || pg.Posts[149].ID != "p149" || pg.End {
		t.Fatalf("150: %v %d %v", err, len(pg.Posts), pg.End)
	}
	pg, err = p.List(ctx, reddit.Query{Target: tg, Limit: 1000})
	if err != nil || len(pg.Posts) != 250 || !pg.End {
		t.Fatalf("all: %v %d %v", err, len(pg.Posts), pg.End)
	}
	// A time range.
	pg, err = p.List(ctx, reddit.Query{Target: tg, Limit: 1000, After: time.Unix(newest-10*60-1, 0), Before: time.Unix(newest-5*60, 0)})
	if err != nil || len(pg.Posts) != 6 || pg.Posts[0].ID != "p5" {
		t.Fatalf("range: %v %+v", err, pg)
	}
	ut, _ := reddit.Parse("https://www.reddit.com/user/limited/")
	_, err = p.List(ctx, reddit.Query{Target: ut, Limit: 10})
	if e := reddit.AsError(err); e.Class != ytdlp.ClassRateLimited || e.RetryAfter != 30*time.Second {
		t.Fatalf("429: %+v", e)
	}
	st, _ := reddit.Parse("https://www.reddit.com/r/slow/")
	if _, err := p.List(ctx, reddit.Query{Target: st, Limit: 10}); reddit.AsError(err).Class != ytdlp.ClassTransient {
		t.Fatalf("timeout: %v", err)
	}
	top, _ := reddit.Parse("https://www.reddit.com/r/big/top/?t=week")
	before := calls
	if _, err := p.List(ctx, reddit.Query{Target: top, Limit: 10}); !reddit.AsError(err).Unsupported || calls != before {
		t.Fatalf("top list: %v", err)
	}
}
