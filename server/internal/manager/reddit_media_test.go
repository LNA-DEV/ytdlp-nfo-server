package manager

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/fakereddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/scenario"
)

// redditMediaWorld is a world with a fake Reddit whose v.redd.it videos
// are fake yt-dlp videos.
func redditMediaWorld(t *testing.T, videos ...string) (*world, *fakereddit.Site) {
	t.Helper()
	site := fakereddit.NewSite(t)
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{}, Aliases: map[string]string{}}
	for _, id := range videos {
		sc.Videos[id] = vid(id, "Clip "+id)
		sc.Aliases[reddit.ManifestURL(id)] = scenario.VideoPrefix + id
	}
	w := newWorld(t, sc)
	w.reddit = &browser.Options{RootCAs: site.Roots, Dial: site.Dial}
	w.startSubs()
	return w, site
}

func (w *world) download(u string, req config.OptionsRequest) *store.Job {
	w.t.Helper()
	j := w.submit(u, req)
	return w.wait(j.ID, store.JobCompleted, store.JobPartial, store.JobFailed)
}

func postURL(sub, id string) string {
	return "https://www.reddit.com/r/" + sub + "/comments/" + id + "/post/"
}

// lib returns the library files below dir (hidden files included).
func (w *world) lib(dir string) []string {
	var out []string
	for _, f := range w.libFiles() {
		if strings.HasPrefix(f, dir) {
			out = append(out, strings.TrimPrefix(f, dir+"/"))
		}
	}
	sort.Strings(out)
	return out
}

func (w *world) archiveFile() string {
	b, _ := os.ReadFile(filepath.Join(w.env.DownloadDir, ".ytdlp-archive.txt"))
	return string(b)
}

func TestRedditPostDownloads(t *testing.T) {
	w, site := redditMediaWorld(t, "vid4")
	created := time.Date(2026, 10, 27, 18, 3, 11, 0, time.UTC)
	site.Add(
		&fakereddit.Post{ID: "p1", Subreddit: "pics", Author: "ann", Title: "Lake", Kind: "image", Media: []string{"img1"}, Created: created},
		&fakereddit.Post{ID: "p2", Subreddit: "pics", Author: "bob", Title: "Spin", Kind: "gif", Media: []string{"gif2"}, Created: created},
		&fakereddit.Post{ID: "p3", Subreddit: "pics", Author: "cy", Title: "Trip", Kind: "gallery", Media: []string{"a1", "a2", "a3"}, Created: created},
		&fakereddit.Post{ID: "p4", Subreddit: "pics", Author: "dee", Title: "Clip", Kind: "video", Media: []string{"vid4"}, Created: created, NSFW: true},
		&fakereddit.Post{ID: "p5", Subreddit: "pics", Author: "eve", Title: "Words", Kind: "text", Created: created},
	)

	j := w.download(postURL("pics", "p1"), config.OptionsRequest{})
	if j.State != store.JobCompleted {
		t.Fatalf("image job: %s %s\n%s", j.State, j.Error, w.log(j.ID))
	}
	its := w.items(j.ID)
	if its[0].ArchiveKey != "redditmedia img1" || its[0].FinalPath != "Lake [p1]" {
		t.Fatalf("image item: %+v", its[0])
	}
	if got := w.lib("Lake [p1]"); !slices.Equal(got, []string{library.MarkerName, "Lake [p1].png"}) {
		t.Fatalf("image folder: %v", got)
	}
	fi, _ := os.Stat(filepath.Join(w.env.LibraryRoot(), "Lake [p1]", "Lake [p1].png"))
	if !fi.ModTime().Equal(created) {
		t.Fatalf("mtime %v, want the post time", fi.ModTime())
	}
	m := library.ReadMarker(filepath.Join(w.env.LibraryRoot(), "Lake [p1]"))
	if m == nil || m.Key != "redditmedia img1" || m.Post != "p1" || m.Provider != "native" || len(m.Files) != 1 || m.Files[0].SHA256 == "" {
		t.Fatalf("marker: %+v", m)
	}

	j = w.download(postURL("pics", "p2"), config.OptionsRequest{})
	if got := w.lib("Spin [p2]"); j.State != store.JobCompleted || !slices.Equal(got, []string{library.MarkerName, "Spin [p2].gif"}) {
		t.Fatalf("gif: %s %v\n%s", j.State, got, w.log(j.ID))
	}

	j = w.download(postURL("pics", "p3"), config.OptionsRequest{})
	want := []string{library.MarkerName, "Trip [p3] - 01.png", "Trip [p3] - 02.png", "Trip [p3] - 03.png"}
	if got := w.lib("Trip [p3]"); j.State != store.JobCompleted || !slices.Equal(got, want) {
		t.Fatalf("gallery: %s %v\n%s", j.State, got, w.log(j.ID))
	}
	if k := w.items(j.ID)[0].ArchiveKey; k != "redditgallery "+reddit.GalleryHash([]reddit.Media{{MediaID: "a1"}, {MediaID: "a2"}, {MediaID: "a3"}}) {
		t.Fatalf("gallery key %q", k)
	}

	j = w.download(postURL("pics", "p4"), config.OptionsRequest{})
	if j.State != store.JobCompleted {
		t.Fatalf("video: %s %s\n%s", j.State, j.Error, w.log(j.ID))
	}
	got := w.lib("Clip [p4]")
	if !slices.Equal(got, []string{library.MarkerName, "Clip [p4].mkv", "folder.jpg", "movie.nfo"}) {
		t.Fatalf("video folder: %v", got)
	}
	nfo, _ := os.ReadFile(filepath.Join(w.env.LibraryRoot(), "Clip [p4]", "movie.nfo"))
	for _, s := range []string{"<title>Clip</title>", "<name>dee</name>", "<mpaa>18</mpaa>", "<premiered>2026-10-27</premiered>"} {
		if !strings.Contains(string(nfo), s) {
			t.Fatalf("NFO lacks %s:\n%s", s, nfo)
		}
	}
	// Only the video key goes to the archive file yt-dlp-compatible tools read.
	if a := w.archiveFile(); !strings.Contains(a, "reddit vid4") || strings.Contains(a, "redditmedia") || strings.Contains(a, "redditgallery") {
		t.Fatalf("archive file:\n%s", a)
	}

	j = w.download(postURL("pics", "p5"), config.OptionsRequest{})
	if j.State != store.JobFailed || !strings.Contains(j.Error, "text post") || j.ErrorClass != "permanent" {
		t.Fatalf("text post: %s %s %s", j.State, j.ErrorClass, j.Error)
	}

	// A pasted subreddit lists its posts: all media is in the library.
	j = w.download("https://www.reddit.com/r/pics/", config.OptionsRequest{})
	if j.State != store.JobCompleted || j.Title != "r/pics" || len(w.items(j.ID)) != 4 {
		t.Fatalf("subreddit job: %s %q %d\n%s", j.State, j.Title, len(w.items(j.ID)), w.log(j.ID))
	}
	for _, it := range w.items(j.ID) {
		if it.State != store.ItemSkipped {
			t.Fatalf("item %s: %s", it.ArchiveKey, it.State)
		}
	}

	// Check library finds the image folders.
	res, err := w.m.CheckArchive(context.Background())
	if err != nil || res.OK != 4 || res.Missing != 0 || res.Incomplete != 0 {
		t.Fatalf("check: %+v %v", res, err)
	}

	// A forced re-download replaces the gallery's folder.
	force := true
	j = w.download(postURL("pics", "p3"), config.OptionsRequest{Force: &force})
	if j.State != store.JobCompleted || len(w.lib("Trip [p3] (2)")) != 0 || len(w.lib("Trip [p3]")) != 4 {
		t.Fatalf("force: %s\n%s", j.State, w.log(j.ID))
	}
}

func TestRedditDuplicates(t *testing.T) {
	w, site := redditMediaWorld(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	orig := &fakereddit.Post{ID: "o1", Subreddit: "pics", Title: "Original", Kind: "image", Media: []string{"img1"}, Created: now}
	gal := &fakereddit.Post{ID: "g1", Subreddit: "pics", Title: "Gallery", Kind: "gallery", Media: []string{"a1", "a2", "a3"}, Created: now}
	site.Add(orig, gal)
	for _, id := range []string{"o1", "g1"} {
		if j := w.download(postURL("pics", id), config.OptionsRequest{}); j.State != store.JobCompleted {
			t.Fatalf("%s: %s\n%s", id, j.State, w.log(j.ID))
		}
	}
	images := func() int { return site.Count("i.redd.it", "/") }
	before := images()

	// Level 1: a crosspost shows the original's media and is skipped
	// without a media download.
	site.Add(&fakereddit.Post{ID: "x1", Subreddit: "other", Title: "Look at this", Original: orig, Created: now})
	j := w.download(postURL("other", "x1"), config.OptionsRequest{})
	it := w.items(j.ID)[0]
	if j.State != store.JobCompleted || it.State != store.ItemSkipped || it.ArchiveKey != "redditmedia img1" || images() != before {
		t.Fatalf("crosspost: %s %+v, %d image requests\n%s", j.State, it, images()-before, w.log(j.ID))
	}

	// Level 2: an identical re-upload under a new media id is downloaded,
	// skipped before finalize and recorded as an alias.
	site.SetImage("img9", fakereddit.MediaBytes("img1", false))
	site.Add(&fakereddit.Post{ID: "r1", Subreddit: "pics", Title: "Repost", Kind: "image", Media: []string{"img9"}, Created: now})
	j = w.download(postURL("pics", "r1"), config.OptionsRequest{})
	it = w.items(j.ID)[0]
	if it.State != store.ItemSkipped || it.Error != "Identical to Original [o1]/Original [o1].png" {
		t.Fatalf("re-upload: %+v\n%s", it, w.log(j.ID))
	}
	if in, _ := w.m.Store.InLibrary(ctx, "redditmedia img9"); !in {
		t.Fatal("no alias for the re-upload")
	}
	if len(w.lib("Repost")) != 0 {
		t.Fatal("re-upload stored")
	}

	// A gallery with images known by media key (img1, the alias img9),
	// one identical to a library file (n2 = a1) and two new ones.
	site.SetImage("n2", fakereddit.MediaBytes("a1", false))
	site.Add(&fakereddit.Post{ID: "g2", Subreddit: "pics", Title: "Mixed", Kind: "gallery",
		Media: []string{"img1", "n1", "img9", "n2", "n3"}, Created: now})
	before = images()
	j = w.download(postURL("pics", "g2"), config.OptionsRequest{})
	it = w.items(j.ID)[0]
	want := []string{library.MarkerName, "Mixed [g2] - 02.png", "Mixed [g2] - 05.png"}
	if got := w.lib("Mixed [g2]"); j.State != store.JobCompleted || !slices.Equal(got, want) {
		t.Fatalf("partly known gallery: %s %v\n%s", j.State, got, w.log(j.ID))
	}
	if n := images() - before; n != 3 { // n1, n2, n3
		t.Fatalf("%d image requests", n)
	}
	if len(it.Warnings) != 1 || !strings.Contains(it.Warnings[0], "3 of 5 images were already in the library") {
		t.Fatalf("warnings: %v", it.Warnings)
	}
	m := library.ReadMarker(filepath.Join(w.env.LibraryRoot(), "Mixed [g2]"))
	if m == nil || len(m.Known) != 3 || m.Known[0].Position != 1 || m.Known[0].Path != "Original [o1]/Original [o1].png" ||
		m.Known[2].Position != 4 || m.Known[2].SHA256 == "" || m.Known[2].Path != "Gallery [g1]/Gallery [g1] - 01.png" {
		t.Fatalf("marker known: %+v", m)
	}
	if row, _ := w.m.Store.MediaByKey(ctx, "redditmedia n2"); row == nil || !row.Alias || row.Path != "Gallery [g1]/Gallery [g1] - 01.png" {
		t.Fatalf("alias n2: %+v", row)
	}

	// A gallery whose images are all in the library is skipped; its key
	// counts as in library.
	site.Add(&fakereddit.Post{ID: "g3", Subreddit: "pics", Title: "Again", Kind: "gallery", Media: []string{"a2", "n1"}, Created: now})
	j = w.download(postURL("pics", "g3"), config.OptionsRequest{})
	it = w.items(j.ID)[0]
	if it.State != store.ItemSkipped || it.Error != "All 2 images are already in the library" {
		t.Fatalf("known gallery: %+v\n%s", it, w.log(j.ID))
	}
	if in, _ := w.m.Store.InLibrary(ctx, it.ArchiveKey); !in {
		t.Fatal("known gallery's key not in library")
	}

	// Force downloads a partly known gallery completely.
	force := true
	j = w.download(postURL("pics", "g2"), config.OptionsRequest{Force: &force})
	if got := w.lib("Mixed [g2]"); j.State != store.JobCompleted || len(got) != 6 {
		t.Fatalf("forced gallery: %v\n%s", got, w.log(j.ID))
	}

	// Removing the original's archive entry frees the re-upload.
	if _, err := w.m.FixArchive(ctx, []string{"redditmedia img1"}, false); err != nil {
		t.Fatal(err)
	}
	if rows, _ := w.m.Store.MediaOf(ctx, "redditmedia img1"); len(rows) != 0 {
		t.Fatalf("rows outlived their archive entry: %+v", rows)
	}
	// img9 is still in the library: the forced gallery stored it.
	if row, _ := w.m.Store.MediaByKey(ctx, "redditmedia img9"); row == nil || row.Alias || !strings.HasPrefix(row.Path, "Mixed [g2]/") {
		t.Fatalf("img9: %+v", row)
	}
}

func TestRedditBrokenMedia(t *testing.T) {
	w, site := redditMediaWorld(t)
	now := time.Now()
	site.SetGone("b2")
	site.SetGone("c1")
	site.SetGone("c2")
	site.SetImage("html", []byte("<!doctype html><title>Removed</title>"))
	site.Add(
		&fakereddit.Post{ID: "bb", Subreddit: "pics", Title: "Some gone", Kind: "gallery", Media: []string{"b1", "b2", "b3"}, Created: now},
		&fakereddit.Post{ID: "cc", Subreddit: "pics", Title: "All gone", Kind: "gallery", Media: []string{"c1", "c2"}, Created: now},
		&fakereddit.Post{ID: "hh", Subreddit: "pics", Title: "Page", Kind: "image", Media: []string{"html"}, Created: now},
		&fakereddit.Post{ID: "dd", Subreddit: "pics", Title: "Deleted", Kind: "image", Media: []string{"d1"}, Created: now, Removed: true},
	)
	j := w.download(postURL("pics", "bb"), config.OptionsRequest{})
	if len(w.items(j.ID)) == 0 {
		t.Fatalf("no items: %s\n%s", j.Error, w.log(j.ID))
	}
	it := w.items(j.ID)[0]
	if got := w.lib("Some gone [bb]"); j.State != store.JobCompleted || !slices.Equal(got, []string{library.MarkerName, "Some gone [bb] - 01.png", "Some gone [bb] - 03.png"}) ||
		len(it.Warnings) != 1 || it.Warnings[0] != "1 of 3 images were removed" {
		t.Fatalf("partly removed: %s %v %v\n%s", j.State, got, it.Warnings, w.log(j.ID))
	}
	for id, reason := range map[string]string{"cc": "all images of the gallery were removed", "hh": "not an image", "dd": "removed"} {
		j = w.download(postURL("pics", id), config.OptionsRequest{})
		if j.State != store.JobFailed || j.ErrorClass != "permanent" || !strings.Contains(j.Error, reason) {
			t.Fatalf("%s: %s %s %s\n%s", id, j.State, j.ErrorClass, j.Error, w.log(j.ID))
		}
	}
}

// A Reddit subscription downloads images, videos and galleries; the
// entries of galleries listed without their media move to the gallery
// key; a crosspost of a downloaded image is in the library at listing
// time.
func TestRedditSubscriptionDownloads(t *testing.T) {
	w, site := redditMediaWorld(t, "vid1", "vid6")
	now := time.Now().Truncate(time.Second)
	site.Add(mixedPosts("EarthPorn", now, 0, 10)...)
	start := time.Now()
	v := w.subscribe(SubscriptionInput{Sources: []SourceInput{{URL: "https://www.reddit.com/r/EarthPorn/"}},
		Mode: strp(store.ModeAuto), Backlog: &store.BacklogChoice{Mode: store.BacklogAll}})
	id := sid(v)
	w.waitChecked(id, start)
	jobs := w.subJobs(id)
	if len(jobs) != 1 {
		t.Fatalf("jobs: %d", len(jobs))
	}
	j := w.wait(jobs[0].ID, store.JobCompleted, store.JobPartial, store.JobFailed)
	if j.State != store.JobCompleted || j.Counts.Done != 6 {
		t.Fatalf("backlog job: %s %+v\n%s", j.State, j.Counts, w.log(j.ID))
	}
	lib := w.entries(id, store.EntryLibrary)
	gallery := 0
	for _, e := range lib {
		if strings.HasPrefix(e.Key, "redditgallery ") { // listed as "redditpost <id>"
			gallery++
		}
	}
	if len(lib) != 6 || gallery != 2 {
		t.Fatalf("in library: %s", keysOf(lib))
	}
	for _, f := range []string{"EarthPorn/Post 0 [p0]/Post 0 [p0].png", "EarthPorn/Post 1 [p1]/Post 1 [p1].mkv",
		"EarthPorn/Post 2 [p2]/Post 2 [p2] - 02.png"} {
		if _, err := os.Stat(filepath.Join(w.env.LibraryRoot(), f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	// The next check lists the same posts: nothing new, no job.
	w.checkNow(id)
	if n := w.newCount(id); n != 0 || len(w.subJobs(id)) != 1 {
		t.Fatalf("second check: %d new, %d jobs", n, len(w.subJobs(id)))
	}

	// A crosspost of a downloaded image in another subscription.
	var orig *fakereddit.Post
	for _, p := range mixedPosts("EarthPorn", now, 0, 1) {
		orig = p
	}
	site.Add(&fakereddit.Post{ID: "xp", Subreddit: "pics", Title: "Crosspost", Original: orig, Created: now})
	start = time.Now()
	v = w.subscribe(SubscriptionInput{Sources: []SourceInput{{URL: "https://www.reddit.com/r/pics/"}}, Mode: strp(store.ModeAuto),
		Backlog: &store.BacklogChoice{Mode: store.BacklogAll}})
	pics := sid(v)
	w.waitChecked(pics, start)
	if got := keysOf(w.entries(pics, store.EntryLibrary)); got != "redditmedia img0" || len(w.subJobs(pics)) != 0 {
		t.Fatalf("crosspost: in library %q, %d jobs", got, len(w.subJobs(pics)))
	}
}

// Level 3: a resized, recompressed repost looks like a library image.
func TestRedditSimilarImages(t *testing.T) {
	w, site := redditMediaWorld(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	site.Add(&fakereddit.Post{ID: "o1", Subreddit: "pics", Title: "Original", Kind: "image", Media: []string{"img1"}, Created: now},
		&fakereddit.Post{ID: "g1", Subreddit: "pics", Title: "Gallery", Kind: "gallery", Media: []string{"a1", "a2"}, Created: now})
	for _, id := range []string{"o1", "g1"} {
		if j := w.download(postURL("pics", id), config.OptionsRequest{}); j.State != store.JobCompleted {
			t.Fatalf("%s: %s\n%s", id, j.State, w.log(j.ID))
		}
	}
	setSimilar := func(mode string) {
		t.Helper()
		if _, err := w.m.Settings.Update(ctx, func(s *config.Settings) error { s.Reddit.Duplicates.Similar = mode; return nil }); err != nil {
			t.Fatal(err)
		}
	}

	// warn (default): finalized, with a warning.
	site.SetImage("rs1", fakereddit.Repost("img1", 100, 75, 70))
	site.Add(&fakereddit.Post{ID: "r1", Subreddit: "pics", Title: "Repost", Kind: "image", Media: []string{"rs1"}, Created: now})
	j := w.download(postURL("pics", "r1"), config.OptionsRequest{})
	it := w.items(j.ID)[0]
	if it.State != store.ItemDone || !slices.Equal(it.Warnings, []string{"Looks like Original [o1]/Original [o1].png"}) {
		t.Fatalf("warn: %s %v\n%s", it.State, it.Warnings, w.log(j.ID))
	}

	// skip: skipped with an alias to the similar file.
	setSimilar(config.SimilarSkip)
	site.SetImage("rs2", fakereddit.Repost("a2", 90, 80, 60))
	site.Add(&fakereddit.Post{ID: "r2", Subreddit: "pics", Title: "Again", Kind: "image", Media: []string{"rs2"}, Created: now})
	j = w.download(postURL("pics", "r2"), config.OptionsRequest{})
	it = w.items(j.ID)[0]
	if it.State != store.ItemSkipped || it.Error != "Looks like Gallery [g1]/Gallery [g1] - 02.png" {
		t.Fatalf("skip: %+v\n%s", it, w.log(j.ID))
	}
	if row, _ := w.m.Store.MediaByKey(ctx, "redditmedia rs2"); row == nil || !row.Alias || row.Path != "Gallery [g1]/Gallery [g1] - 02.png" {
		t.Fatalf("alias: %+v", row)
	}

	// A gallery with a similar image stores only the other one.
	site.SetImage("gs1", fakereddit.Repost("a1", 110, 80, 65))
	site.Add(&fakereddit.Post{ID: "g2", Subreddit: "pics", Title: "Mixed", Kind: "gallery", Media: []string{"gs1", "new1"}, Created: now})
	j = w.download(postURL("pics", "g2"), config.OptionsRequest{})
	if got := w.lib("Mixed [g2]"); j.State != store.JobCompleted || !slices.Equal(got, []string{library.MarkerName, "Mixed [g2] - 02.png"}) {
		t.Fatalf("gallery: %v\n%s", got, w.log(j.ID))
	}
	m := library.ReadMarker(filepath.Join(w.env.LibraryRoot(), "Mixed [g2]"))
	if m == nil || len(m.Known) != 1 || !m.Known[0].Similar || m.Known[0].Path != "Gallery [g1]/Gallery [g1] - 01.png" || m.Files[0].DHash == "" {
		t.Fatalf("marker: %+v", m)
	}
	if row, _ := w.m.Store.MediaByKey(ctx, "redditmedia gs1"); row == nil || !row.Alias {
		t.Fatalf("gallery alias: %+v", row)
	}

	// off: no hash, no warning.
	setSimilar(config.SimilarOff)
	site.SetImage("rs3", fakereddit.Repost("img1", 96, 72, 50))
	site.Add(&fakereddit.Post{ID: "r3", Subreddit: "pics", Title: "Third", Kind: "image", Media: []string{"rs3"}, Created: now})
	j = w.download(postURL("pics", "r3"), config.OptionsRequest{})
	if it = w.items(j.ID)[0]; it.State != store.ItemDone || len(it.Warnings) != 0 {
		t.Fatalf("off: %s %v", it.State, it.Warnings)
	}
}

// linkWorld is a fake Reddit plus fake yt-dlp videos behind YouTube and
// Redgifs links (Reddit concept §7.6).
func linkWorld(t *testing.T) (*world, *fakereddit.Site) {
	t.Helper()
	site := fakereddit.NewSite(t)
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{}, Aliases: map[string]string{}}
	for _, v := range []struct{ id, extractor, url string }{
		{"dQw4w9WgXcQ", "Youtube", "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
		{"zzzzzzzzzzz", "Youtube", "https://www.youtube.com/watch?v=zzzzzzzzzzz"},
		{"happyloudfox", "RedGifs", "https://www.redgifs.com/watch/happyloudfox"},
	} {
		sv := vid(v.id, "Target "+v.id)
		sv.ExtractorKey = v.extractor
		sc.Videos[v.id] = sv
		sc.Aliases[v.url] = scenario.VideoPrefix + v.id
	}
	w := newWorld(t, sc)
	w.reddit = &browser.Options{RootCAs: site.Roots, Dial: site.Dial}
	w.startSubs()
	return w, site
}

func TestRedditLinks(t *testing.T) {
	w, site := linkWorld(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	link := func(id, title, target string, minutes int) *fakereddit.Post {
		return &fakereddit.Post{ID: id, Subreddit: "mixed", Title: title, Kind: "link", Link: target, Created: now.Add(-time.Duration(minutes) * time.Minute)}
	}
	site.Add(
		&fakereddit.Post{ID: "p1", Subreddit: "mixed", Title: "Photo", Kind: "image", Media: []string{"img1"}, Created: now},
		link("y1", "A song", "https://youtu.be/dQw4w9WgXcQ?si=share", 1),
		link("y2", "Known song", "https://www.youtube.com/watch?v=abcdefghijk", 2),
		link("rg", "A clip", "https://www.redgifs.com/watch/HappyLoudFox", 3),
		link("im", "A picture", "https://i.imgur.com/AbCdE12.jpg", 4),
		link("vm", "Elsewhere", "https://vimeo.com/123456", 5),
	)
	// A video already downloaded from YouTube itself.
	w.m.Store.ImportArchiveKeys(ctx, []string{"youtube abcdefghijk"}, "test")

	start := time.Now()
	v := w.subscribe(SubscriptionInput{Sources: []SourceInput{{URL: "https://www.reddit.com/r/mixed/"}}, Mode: strp(store.ModeAuto),
		Backlog: &store.BacklogChoice{Mode: store.BacklogAll}})
	id := sid(v)
	w.waitChecked(id, start)
	all := map[string]EntryView{}
	for _, e := range w.entries(id, "") {
		all[e.Key] = e
	}
	if e := all["youtube dQw4w9WgXcQ"]; e.MediaKind != "linked" || e.URL != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" || e.Title != "A song" {
		t.Fatalf("YouTube entry: %+v (all: %v)", e, keysOf(w.entries(id, "")))
	}
	if e := all["youtube abcdefghijk"]; e.Status != store.EntryLibrary {
		t.Fatalf("known video: %+v", e)
	}
	if e := all["imgur AbCdE12"]; e.MediaKind != "linked" || !strings.Contains(e.URL, "/comments/im/") {
		t.Fatalf("Imgur entry: %+v", e)
	}
	if e := all["redditpost vm"]; e.Status != store.EntryIgnored || e.IgnoreReason != "link to vimeo.com" {
		t.Fatalf("Vimeo entry: %+v", e)
	}

	// One job per site: Reddit (the photo and the Imgur image), YouTube,
	// Redgifs; the known video gets none.
	jobs := w.subJobs(id)
	sites := map[string]*store.Job{}
	for _, j := range jobs {
		sites[j.Site] = w.wait(j.ID, store.JobCompleted, store.JobPartial, store.JobFailed)
	}
	if len(jobs) != 3 || sites["reddit.com"] == nil || sites["youtube.com"] == nil || sites["redgifs.com"] == nil {
		t.Fatalf("jobs: %d %v", len(jobs), sites)
	}
	for site, j := range sites {
		if j.State != store.JobCompleted {
			t.Fatalf("%s job: %s %s\n%s", site, j.State, j.Error, w.log(j.ID))
		}
	}
	yt := w.items(sites["youtube.com"].ID)
	if len(yt) != 1 || yt[0].ArchiveKey != "youtube dQw4w9WgXcQ" || !strings.HasPrefix(yt[0].FinalPath, "mixed/Target dQw4w9WgXcQ [dQw4w9WgXcQ]") {
		t.Fatalf("YouTube item: %+v", yt)
	}
	if rg := w.items(sites["redgifs.com"].ID); rg[0].ArchiveKey != "redgifs happyloudfox" {
		t.Fatalf("Redgifs item: %+v", rg[0])
	}
	if got := w.lib("mixed/A picture [im]"); !slices.Equal(got, []string{library.MarkerName, "A picture [im].jpg"}) {
		t.Fatalf("Imgur image: %v", got)
	}
	// yt-dlp-compatible tools learn the linked videos and the Imgur key.
	if a := w.archiveFile(); !strings.Contains(a, "youtube dQw4w9WgXcQ") || !strings.Contains(a, "redgifs happyloudfox") {
		t.Fatalf("archive file:\n%s", a)
	}

	// Adding a host follows its link posts that were left out.
	if _, err := w.m.Settings.Update(ctx, func(s *config.Settings) error {
		s.Reddit.ExternalHosts = append(s.Reddit.ExternalHosts, "vimeo.com")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w.checkNow(id)
	for _, e := range w.entries(id, "") {
		if strings.HasSuffix(e.URL, "vimeo.com/123456") && e.Status == store.EntryIgnored {
			t.Fatalf("vimeo entry still ignored: %+v", e)
		}
	}
}

// A pasted link post downloads its target; a link to a site off the list
// fails with a reason.
func TestRedditPastedLinkPost(t *testing.T) {
	w, site := linkWorld(t)
	site.Add(&fakereddit.Post{ID: "lp", Subreddit: "music", Title: "Listen", Kind: "link", Link: "https://www.youtube.com/watch?v=zzzzzzzzzzz", Created: time.Now()},
		&fakereddit.Post{ID: "lv", Subreddit: "music", Title: "Vimeo", Kind: "link", Link: "https://vimeo.com/1", Created: time.Now()})
	j := w.download(postURL("music", "lp"), config.OptionsRequest{})
	its := w.items(j.ID)
	if j.State != store.JobCompleted || its[0].ArchiveKey != "youtube zzzzzzzzzzz" || its[0].URL != "https://www.youtube.com/watch?v=zzzzzzzzzzz" {
		t.Fatalf("link post: %s %+v\n%s", j.State, its[0], w.log(j.ID))
	}
	j = w.download(postURL("music", "lv"), config.OptionsRequest{})
	if j.State != store.JobFailed || !strings.Contains(j.Error, "not on the list of followed sites") {
		t.Fatalf("vimeo: %s %s", j.State, j.Error)
	}
}

// i.redd.it serves an upload only under its own extension: .jpeg images
// and galleries load through both providers, and a JPEG whose extension
// only its MIME type named is found under the other spelling.
func TestRedditJPEGExtensions(t *testing.T) {
	w, site := redditMediaWorld(t)
	now := time.Now().Truncate(time.Second)
	site.Add(
		&fakereddit.Post{ID: "ij", Subreddit: "example", Title: "Jpeg", Kind: "image", Media: []string{"jp1"}, Ext: "jpeg", Created: now},
		&fakereddit.Post{ID: "gj", Subreddit: "example", Title: "Jpeg gallery", Kind: "gallery", Media: []string{"jg1", "jg2"}, Ext: "jpeg", Created: now},
		&fakereddit.Post{ID: "gn", Subreddit: "example", Title: "No preview", Kind: "gallery", Media: []string{"jn1", "jn2"}, Ext: "jpeg",
			NoPreview: true, Created: now},
	)
	check := func(id, folder string, want ...string) {
		t.Helper()
		j := w.download(postURL("example", id), config.OptionsRequest{})
		if got := w.lib(folder); j.State != store.JobCompleted || !slices.Equal(got, append([]string{library.MarkerName}, want...)) {
			t.Fatalf("%s: %s %v\n%s", id, j.State, got, w.log(j.ID))
		}
	}
	check("ij", "Jpeg [ij]", "Jpeg [ij].jpg")
	check("gj", "Jpeg gallery [gj]", "Jpeg gallery [gj] - 01.jpg", "Jpeg gallery [gj] - 02.jpg")
	// Native knows the uploaded extension: no detour through a 404.
	if n := site.Count("i.redd.it", "/jp1.jpg") + site.Count("i.redd.it", "/jg1.jpg"); n != 0 {
		t.Fatalf("%d requests with the wrong extension", n)
	}
	site.SetNativeBlocked(true) // Arctic Shift: the extension comes from the preview URL or the MIME type
	check("gn", "No preview [gn]", "No preview [gn] - 01.jpg", "No preview [gn] - 02.jpg")
	if n := site.Count("i.redd.it", "/jn1.jpg"); n != 1 {
		t.Fatalf("%d requests for the MIME spelling", n)
	}
}
