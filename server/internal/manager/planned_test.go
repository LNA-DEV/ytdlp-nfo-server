package manager

import (
	"context"
	"strings"
	"testing"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/scenario"
)

// planned creates a job with pre-planned items, as a subscription does, and
// hands it to the scheduler.
func (w *world) planned(band store.Band, url string, urls ...string) *store.Job {
	w.t.Helper()
	ctx := context.Background()
	s := w.m.Settings.Settings()
	opts, err := config.Resolve(config.OptionsRequest{}, &s, w.m.siteRules(), url)
	if err != nil {
		w.t.Fatal(err)
	}
	j := &store.Job{URL: url, Site: w.m.siteGroup(url, opts), State: store.JobQueued, Options: opts, Band: band}
	if err := w.m.Store.CreateJob(ctx, j); err != nil {
		w.t.Fatal(err)
	}
	var items []*store.Item
	for i, u := range urls {
		id := u[strings.LastIndex(u, "/")+1:]
		items = append(items, &store.Item{JobID: j.ID, Index: i, URL: u, Extractor: "Fake", VideoID: id,
			ArchiveKey: "fake " + id, Title: id, PlaylistTitle: "Sub", State: store.ItemPending})
	}
	if err := w.m.Store.CreateItems(ctx, items); err != nil {
		w.t.Fatal(err)
	}
	w.m.Sched.Enqueue(scheduler.Job{ID: j.ID, Pos: j.QueuePos, Site: j.Site, Band: j.Band})
	return j
}

// A background job gives its slot to an interactive job of the same site
// between two items, then continues (subscriptions concept §12).
func TestPlannedJobYieldsBetweenItems(t *testing.T) {
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{"x": vid("x", "X")}}
	var urls []string
	for _, id := range []string{"b1", "b2", "b3", "b4", "b5"} {
		v := vid(id, strings.ToUpper(id))
		v.Download = []scenario.Behavior{{SleepMs: 150}}
		sc.Videos[id] = v
		urls = append(urls, scenario.VideoPrefix+id)
	}
	w := newWorld(t, sc)
	m := w.start()
	if _, err := m.SaveSiteRule(context.Background(), config.SiteRule{Pattern: "fake.test", MaxConcurrent: 1}); err != nil {
		t.Fatal(err)
	}
	bg := w.planned(store.BandAuto, scenario.PlaylistPrefix+"p", urls...)
	waitItemState(t, m, bg.ID, store.ItemDownloading)
	x := w.submit(scenario.VideoPrefix+"x", config.OptionsRequest{})
	x = w.wait(x.ID, store.JobCompleted)
	bg = w.wait(bg.ID, store.JobCompleted)
	its := w.items(bg.ID)
	if !x.FinishedAt.After(its[0].FinishedAt) || !x.FinishedAt.Before(its[1].FinishedAt) {
		t.Fatalf("interactive job finished at %v, want between item 1 (%v) and item 2 (%v)",
			x.FinishedAt, its[0].FinishedAt, its[1].FinishedAt)
	}
	if n := strings.Count(w.log(bg.ID), "Giving the slot to a waiting download"); n != 1 {
		t.Fatalf("yielded %d times:\n%s", n, w.log(bg.ID))
	}
	for _, it := range its {
		if it.State != store.ItemDone || !strings.HasPrefix(it.FinalPath, "Sub/") {
			t.Fatalf("item %s: %s %s", it.VideoID, it.State, it.FinalPath)
		}
	}
}

// Subscription jobs never probe or expand their URL (§4.3).
func TestPlannedJobNeverProbesOrExpands(t *testing.T) {
	sc := &scenario.Scenario{
		Videos:    map[string]scenario.Video{"a": vid("a", "A")},
		Playlists: map[string]scenario.Playlist{"p": {ID: "p", Title: "List", Entries: []string{"a"}}},
	}
	w := newWorld(t, sc)
	w.start()
	empty := w.planned(store.BandAuto, scenario.PlaylistPrefix+"p")
	empty = w.wait(empty.ID, store.JobFailed)
	if empty.Error != "the job has no items" || w.calls("probe-pl-p") != 0 {
		t.Fatalf("empty job: %q, probes %d", empty.Error, w.calls("probe-pl-p"))
	}
	pl := w.planned(store.BandInteractive, scenario.PlaylistPrefix+"p", scenario.PlaylistPrefix+"p")
	pl = w.wait(pl.ID, store.JobFailed)
	its := w.items(pl.ID)
	if len(its) != 1 || its[0].ErrorClass != "permanent" || !strings.HasPrefix(its[0].Error, "not a single video") {
		t.Fatalf("playlist item: %d items, %+v", len(its), its[0])
	}
	if w.calls("download-a") != 0 {
		t.Fatal("the playlist was expanded")
	}
}
