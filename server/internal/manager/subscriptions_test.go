package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/subscriptions"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/scenario"
)

const chanID = "UC0123456789abcdefghij_-"

var chanVideos = subscriptions.ChannelTabURL(chanID, subscriptions.TabVideos)

// channel adds a fake YouTube-style channel tab with the given listings
// (one per check) and videos v1..vN.
func channel(sc *scenario.Scenario, n int, listings ...[]string) {
	if sc.Videos == nil {
		sc.Videos = map[string]scenario.Video{}
	}
	for i := 1; i <= n; i++ {
		id := fmt.Sprint("v", i)
		sc.Videos[id] = vid(id, "Video "+id)
	}
	if sc.Playlists == nil {
		sc.Playlists = map[string]scenario.Playlist{}
	}
	sc.Playlists["chan"] = scenario.Playlist{ID: chanID, Title: "Chan - Videos", ChannelID: chanID, Uploader: "Chan",
		ExtractorKey: "YoutubeTab", Listings: listings}
	if sc.Aliases == nil {
		sc.Aliases = map[string]string{}
	}
	sc.Aliases[chanVideos] = scenario.PlaylistPrefix + "chan"
}

// newest returns "v<to>", …, "v<from>" (a channel tab lists newest first).
func newest(from, to int) []string {
	var out []string
	for i := to; i >= from; i-- {
		out = append(out, fmt.Sprint("v", i))
	}
	return out
}

func (w *world) startSubs() *Manager {
	m := w.start()
	if _, err := m.Settings.Update(context.Background(), func(s *config.Settings) error {
		s.Subscriptions.SiteDelaySeconds = 0
		return nil
	}); err != nil {
		w.t.Fatal(err)
	}
	return m
}

func strp(s string) *string { return &s }

func (w *world) subscribe(in SubscriptionInput) *SubscriptionView {
	w.t.Helper()
	v, err := w.m.CreateSubscription(context.Background(), in)
	if err != nil {
		w.t.Fatalf("subscribe: %v", err)
	}
	return v
}

func sid(v *SubscriptionView) int64 {
	var id int64
	fmt.Sscan(v.ID, &id)
	return id
}

// waitChecked waits until a check of the subscription finished after since.
func (w *world) waitChecked(id int64, since time.Time) *store.Subscription {
	w.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		sub, err := w.m.Store.GetSubscription(context.Background(), id)
		if err == nil && !sub.LastCheckAt.Before(since) && sub.State != store.SubChecking && !w.m.subs.loop.Running(id) {
			return sub
		}
		if time.Now().After(deadline) {
			log, _ := w.m.subs.logs.Read(id)
			w.t.Fatalf("subscription %d was not checked (%+v)\n%s", id, sub, log)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// checkNow runs a check and waits for it.
func (w *world) checkNow(id int64) *store.Subscription {
	w.t.Helper()
	since := time.Now()
	if err := w.m.CheckSubscription(context.Background(), id, false); err != nil {
		w.t.Fatal(err)
	}
	return w.waitChecked(id, since)
}

func (w *world) entries(id int64, status string) []EntryView {
	w.t.Helper()
	es, err := w.m.ListEntries(context.Background(), EntryListFilter{SubscriptionID: id, Status: status, Limit: 500})
	if err != nil {
		w.t.Fatal(err)
	}
	return es
}

func entryIDs(es []EntryView) string {
	var out []string
	for _, e := range es {
		out = append(out, strings.TrimPrefix(e.Key, "fake "))
	}
	return strings.Join(out, ",")
}

func (w *world) subJobs(id int64) []*store.Job {
	w.t.Helper()
	jobs, err := w.m.Store.ListJobs(context.Background(), store.JobFilter{})
	if err != nil {
		w.t.Fatal(err)
	}
	var out []*store.Job
	for _, j := range jobs {
		if j.SubscriptionID == id {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID }) // oldest first
	return out
}

func itemKeys(its []*store.Item) string {
	var out []string
	for _, it := range its {
		out = append(out, it.VideoID)
	}
	return strings.Join(out, ",")
}

// §12: first check records the backlog, a later check queues the new
// videos in one auto job (oldest first, into the subscription's folder),
// a new video already in the archive is "in library".
func TestSubscriptionAutoMode(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 32, newest(1, 30), append([]string{"v32", "v31", "x"}, newest(1, 30)...))
	sc.Videos["x"] = vid("x", "Archived")
	w := newWorld(t, sc)
	m := w.startSubs()
	ctx := context.Background()
	m.Store.ImportArchiveKeys(ctx, []string{"fake x"}, "test")
	start := time.Now()
	v := w.subscribe(SubscriptionInput{Name: strp("Chan"), Mode: strp(store.ModeAuto), Sources: []SourceInput{{URL: chanVideos}}})
	id := sid(v)
	sub := w.waitChecked(id, start)
	if n := len(w.entries(id, store.EntryBacklog)); n != 30 || len(w.subJobs(id)) != 0 {
		t.Fatalf("first check: %d backlog entries, %d jobs", n, len(w.subJobs(id)))
	}
	if sub.Kind != "channel" || sub.Uploader != "Chan" || sub.Sources[0].Tab != "videos" || sub.Sources[0].FirstOKAt.IsZero() {
		t.Fatalf("subscription after the first check: %+v %+v", sub, sub.Sources[0])
	}

	w.checkNow(id)
	jobs := w.subJobs(id)
	if len(jobs) != 1 || jobs[0].Band != store.BandAuto || jobs[0].Title != "Chan: 2 new videos" || jobs[0].NormURL != "" {
		t.Fatalf("jobs after the second check: %+v", jobs)
	}
	j := w.wait(jobs[0].ID, store.JobCompleted)
	its := w.items(j.ID)
	if itemKeys(its) != "v31,v32" {
		t.Fatalf("items must run oldest first: %s", itemKeys(its))
	}
	for _, it := range its {
		if it.PlaylistTitle != "Chan" || !strings.HasPrefix(it.FinalPath, "Chan/") {
			t.Fatalf("item %s: %q %q", it.VideoID, it.PlaylistTitle, it.FinalPath)
		}
	}
	if got := entryIDs(w.entries(id, store.EntryLibrary)); got != "v32,v31,x" {
		t.Fatalf("in library: %s", got)
	}
	if n := len(w.entries(id, "inbox")); n != 0 {
		t.Fatalf("inbox: %d", n)
	}
	log, _ := m.subs.logs.Read(id)
	if !strings.Contains(string(log), fmt.Sprintf("Checked 1 source: 33 listed, 3 new, 2 queued (job #%d)", j.ID)) {
		t.Fatalf("check log:\n%s", log)
	}
}

// §12: manual mode records an inbox; Download creates one interactive job,
// a double click creates exactly one job, Ignore works.
func TestSubscriptionManualMode(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 5, newest(1, 3), newest(1, 5))
	w := newWorld(t, sc)
	w.startSubs()
	ctx := context.Background()
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Sources: []SourceInput{{URL: chanVideos}}}))
	w.waitChecked(id, start)
	w.checkNow(id)
	inbox := w.entries(id, "inbox")
	if entryIDs(inbox) != "v5,v4" || len(w.subJobs(id)) != 0 {
		t.Fatalf("manual mode: inbox %s, %d jobs", entryIDs(inbox), len(w.subJobs(id)))
	}
	res, err := w.m.DownloadEntries(ctx, DownloadEntriesRequest{IDs: []string{inbox[0].ID, inbox[1].ID}})
	if err != nil || len(res.Jobs) != 1 || res.Queued != 2 {
		t.Fatalf("download: %+v %v", res, err)
	}
	again, err := w.m.DownloadEntries(ctx, DownloadEntriesRequest{IDs: []string{inbox[0].ID}})
	if err != nil || len(again.Jobs) != 0 || again.Skipped != 1 {
		t.Fatalf("double click: %+v %v", again, err)
	}
	j := w.wait(mustID(res.Jobs[0].ID), store.JobCompleted)
	if j.Band != store.BandInteractive || j.Title != "Chan: 2 videos" {
		t.Fatalf("job: band %v title %q", j.Band, j.Title)
	}
	if n, err := w.m.IgnoreEntries(ctx, []string{w.entries(id, store.EntryBacklog)[0].ID}, true); err != nil || n != 1 {
		t.Fatalf("ignore: %d %v", n, err)
	}
	if len(w.entries(id, store.EntryIgnored)) != 1 {
		t.Fatal("ignored entry missing")
	}
}

func mustID(s string) int64 {
	var id int64
	fmt.Sscan(s, &id)
	return id
}

// §12: more new entries than autoQueueCap queue nothing.
func TestSubscriptionCap(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 30, newest(1, 1), newest(1, 30))
	w := newWorld(t, sc)
	w.startSubs()
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Mode: strp(store.ModeAuto), Sources: []SourceInput{{URL: chanVideos}}}))
	w.waitChecked(id, start)
	sub := w.checkNow(id)
	if len(w.subJobs(id)) != 0 || sub.Warning != "Found 29 new videos (limit 25). Review them before downloading." {
		t.Fatalf("cap: %d jobs, warning %q", len(w.subJobs(id)), sub.Warning)
	}
	if n := len(w.entries(id, "inbox")); n != 29 {
		t.Fatalf("inbox %d", n)
	}
	v, err := w.m.UpdateSubscription(context.Background(), id, SubscriptionInput{DismissWarning: true})
	if err != nil || v.Warning != "" {
		t.Fatalf("dismiss: %v %q", err, v.Warning)
	}
}

// §12: a gap larger than the window doubles it until a known entry
// appears.
func TestSubscriptionWindowGap(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 14, newest(1, 2), newest(1, 14))
	w := newWorld(t, sc)
	m := w.startSubs()
	m.Settings.Update(context.Background(), func(s *config.Settings) error { s.Subscriptions.Window = 10; return nil })
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Sources: []SourceInput{{URL: chanVideos}}}))
	w.waitChecked(id, start)
	w.checkNow(id)
	if n := len(w.entries(id, "inbox")); n != 12 {
		t.Fatalf("inbox %d, want all 12 new videos", n)
	}
	log, _ := m.subs.logs.Read(id)
	if !strings.Contains(string(log), "(newest 10)") || !strings.Contains(string(log), "(newest 20)") {
		t.Fatalf("window not doubled:\n%s", log)
	}
}

// §12: permanent failures are never queued again (S3); "not available yet"
// is queued again by every check as a new one-item job.
func TestSubscriptionFailuresAndPremieres(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 3, newest(1, 1), newest(1, 3))
	priv := sc.Videos["v2"]
	priv.Meta = []scenario.Behavior{{Exit: 1, Stderr: []string{"ERROR: [fake] v2: Private video. Sign in if you've been granted access to this video"}}}
	sc.Videos["v2"] = priv
	prem := sc.Videos["v3"]
	prem.Meta = []scenario.Behavior{{Exit: 1, Stderr: []string{"ERROR: [fake] v3: Premieres in 3 hours"}}, {}}
	sc.Videos["v3"] = prem
	w := newWorld(t, sc)
	w.startSubs()
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Mode: strp(store.ModeAuto), Sources: []SourceInput{{URL: chanVideos}}}))
	w.waitChecked(id, start)
	w.checkNow(id)
	jobs := w.subJobs(id)
	if len(jobs) != 1 {
		t.Fatalf("jobs %d", len(jobs))
	}
	w.wait(jobs[0].ID, store.JobFailed)
	if got := entryIDs(w.entries(id, store.EntryFailed)); got != "v2" {
		t.Fatalf("failed: %s", got)
	}
	if got := entryIDs(w.entries(id, store.EntryUpcoming)); got != "v3" {
		t.Fatalf("upcoming: %s", got)
	}
	w.checkNow(id)
	jobs = w.subJobs(id)
	if len(jobs) != 2 || jobs[1].Title != "Chan: Video v3" || jobs[1].Counts.Total != 1 {
		t.Fatalf("re-queue: %d jobs %+v", len(jobs), jobs[len(jobs)-1])
	}
	w.wait(jobs[1].ID, store.JobCompleted)
	if got := entryIDs(w.entries(id, store.EntryLibrary)); got != "v3" {
		t.Fatalf("library: %s", got)
	}
	w.checkNow(id)
	if n := len(w.subJobs(id)); n != 2 {
		t.Fatalf("the private video was queued again: %d jobs", n)
	}
	// Retry on the failed entry: a new one-item job.
	res, err := w.m.DownloadEntries(context.Background(), DownloadEntriesRequest{IDs: []string{w.entries(id, store.EntryFailed)[0].ID}})
	if err != nil || len(res.Jobs) != 1 || res.Jobs[0].Band != "interactive" {
		t.Fatalf("retry: %+v %v", res, err)
	}
}

// §12: post_live and is_upcoming in the listing wait; the first check that
// no longer reports them queues the entry.
func TestSubscriptionUpcomingFromListing(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 3, newest(1, 1), []string{"v3|live_status=is_upcoming", "v2|live_status=post_live", "v1"},
		[]string{"v3|live_status=is_upcoming", "v2|live_status=was_live", "v1"}, []string{"v3|live_status=not_live", "v2", "v1"})
	w := newWorld(t, sc)
	w.startSubs()
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Mode: strp(store.ModeAuto), Sources: []SourceInput{{URL: chanVideos}}}))
	w.waitChecked(id, start)
	w.checkNow(id)
	if len(w.subJobs(id)) != 0 || len(w.entries(id, store.EntryUpcoming)) != 2 {
		t.Fatalf("upcoming queued: %d jobs", len(w.subJobs(id)))
	}
	w.checkNow(id)
	jobs := w.subJobs(id)
	if len(jobs) != 1 || jobs[0].Title != "Chan: Video v2" {
		t.Fatalf("finished stream: %+v", jobs)
	}
	w.checkNow(id)
	if jobs = w.subJobs(id); len(jobs) != 2 || jobs[1].Title != "Chan: Video v3" {
		t.Fatalf("premiere: %d jobs", len(jobs))
	}
}

// §12: HTTP 429 sets the site cooldown (downloads of the site pause too)
// and postpones the check.
func TestSubscriptionRateLimited(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 1, newest(1, 1))
	pl := sc.Playlists["chan"]
	pl.Probe = []scenario.Behavior{{Exit: 1, Stderr: []string{"ERROR: [youtube:tab] x: Unable to download webpage: HTTP Error 429: Too Many Requests"}}, {}}
	sc.Playlists["chan"] = pl
	w := newWorld(t, sc)
	m := w.startSubs()
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Sources: []SourceInput{{URL: chanVideos}}}))
	sub := w.waitChecked(id, start)
	if sub.ErrorClass != "rate_limited" || !sub.Sources[0].FirstOKAt.IsZero() {
		t.Fatalf("after 429: %+v", sub)
	}
	cd := m.Sched.Status().Cooldowns["youtube.com"]
	if cd.IsZero() || sub.NextCheckAt.Before(cd.Add(-time.Second)) {
		t.Fatalf("cooldown %v, next check %v", cd, sub.NextCheckAt)
	}
}

// §12: removed library → the check records nothing; restored → the next
// check finds and queues the new videos. A second library with a folder.
func TestSubscriptionLibraryAndFolder(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 3, newest(1, 1), newest(1, 2), newest(1, 3))
	w := newWorld(t, sc)
	m := w.startSubs()
	ctx := context.Background()
	kids := w.dir + "/kinder"
	setLibs := func(libs []config.Library) {
		if _, err := m.Settings.Update(ctx, func(s *config.Settings) error { s.Libraries = libs; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	setLibs([]config.Library{{Name: "Kinder", Path: kids}})
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Folder: strp("X"), Mode: strp(store.ModeAuto),
		Options: &config.OptionsRequest{Library: strp("Kinder")}, Sources: []SourceInput{{URL: chanVideos}}}))
	w.waitChecked(id, start)
	setLibs(nil)
	sub := w.checkNow(id)
	if sub.Error != `unknown library "Kinder"` || len(w.entries(id, "")) != 1 {
		t.Fatalf("removed library: %q, %d entries", sub.Error, len(w.entries(id, "")))
	}
	setLibs([]config.Library{{Name: "Kinder", Path: kids}})
	w.checkNow(id)
	jobs := w.subJobs(id)
	if len(jobs) != 1 {
		t.Fatalf("after restoring the library: %d jobs", len(jobs))
	}
	j := w.wait(jobs[0].ID, store.JobCompleted)
	it := w.items(j.ID)[0]
	if it.Library != "Kinder" || !strings.HasPrefix(it.FinalPath, "X/") {
		t.Fatalf("item: %s %s", it.Library, it.FinalPath)
	}
	if _, err := m.UpdateSubscription(ctx, id, SubscriptionInput{Folder: strp("Y")}); err != nil {
		t.Fatal(err)
	}
	w.checkNow(id)
	jobs = w.subJobs(id)
	j = w.wait(jobs[len(jobs)-1].ID, store.JobCompleted)
	if it := w.items(j.ID)[0]; !strings.HasPrefix(it.FinalPath, "Y/") {
		t.Fatalf("after the folder change: %s", it.FinalPath)
	}
}

// §12: the same channel under another URL is a conflict (409).
func TestSubscriptionCanonicalConflict(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 1, newest(1, 1))
	w := newWorld(t, sc)
	w.startSubs()
	w.subscribe(SubscriptionInput{Name: strp("Chan"), Sources: []SourceInput{{URL: chanVideos}}})
	_, err := w.m.CreateSubscription(context.Background(), SubscriptionInput{Name: strp("Again"),
		Sources: []SourceInput{{URL: "https://www.youtube.com/@chan/videos", Canonical: chanVideos}}})
	var c *SubscriptionConflict
	if !errors.As(err, &c) || c.Name != "Chan" {
		t.Fatalf("want a conflict with Chan, got %v", err)
	}
	if _, err := w.m.CreateSubscription(context.Background(), SubscriptionInput{Name: strp("Bad"),
		Sources: []SourceInput{{URL: "https://www.youtube.com/@chan/videos", Canonical: "https://evil.test/x"}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign canonical accepted: %v", err)
	}
}

// §12: preview of a channel offers its tabs; a video URL switches to its
// channel; a video without a channel is unprocessable.
func TestSubscriptionPreview(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 12, newest(1, 12))
	v := sc.Videos["v12"]
	v.ChannelURL = "https://www.youtube.com/@chan"
	sc.Videos["v12"] = v
	sc.Aliases["https://www.youtube.com/@chan/videos"] = scenario.PlaylistPrefix + "chan"
	w := newWorld(t, sc)
	m := w.startSubs()
	ctx := context.Background()
	m.Store.ImportArchiveKeys(ctx, []string{"fake v12"}, "test")
	p, err := m.PreviewSubscription(ctx, scenario.VideoPrefix+"v12")
	if err != nil {
		t.Fatal(err)
	}
	if p.ResolvedFrom != "Video v12" || p.Name != "Chan" || p.Kind != "channel" || len(p.Entries) != 10 || p.InLibrary != 1 {
		t.Fatalf("preview: %+v", p)
	}
	if len(p.Sources) != 3 || !p.Sources[0].Selected || p.Sources[0].Canonical != chanVideos ||
		p.Sources[1].Canonical != subscriptions.ChannelTabURL(chanID, "shorts") {
		t.Fatalf("tabs: %+v", p.Sources)
	}
	if _, err := m.PreviewSubscription(ctx, scenario.VideoPrefix+"v1"); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("video without channel: %v", err)
	}
}

// §12: a blocked listing that succeeds with impersonation keeps it.
func TestSubscriptionBlockedUsesImpersonation(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 1, newest(1, 1))
	pl := sc.Playlists["chan"]
	pl.Probe = []scenario.Behavior{{RequireImpersonate: true}}
	sc.Playlists["chan"] = pl
	w := newWorld(t, sc)
	w.startSubs()
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Sources: []SourceInput{{URL: chanVideos}}}))
	sub := w.waitChecked(id, start)
	if sub.ErrorClass != "blocked" {
		t.Fatalf("first check: %+v", sub)
	}
	sub = w.checkNow(id)
	if sub.State != store.SubIdle || !sub.Sources[0].Impersonate {
		t.Fatalf("with impersonation: %+v %+v", sub, sub.Sources[0])
	}
	sub = w.checkNow(id)
	if sub.State != store.SubIdle {
		t.Fatalf("later check: %+v", sub)
	}
}

// §12: a source added later records its backlog and queues nothing; a
// subscription deleted during its auto job leaves the job running.
func TestSubscriptionAddSourceAndDelete(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 2, newest(1, 1), newest(1, 2))
	for i := 1; i <= 40; i++ {
		id := fmt.Sprint("s", i)
		sc.Videos[id] = vid(id, "Short "+id)
	}
	var shorts []string
	for i := 40; i >= 1; i-- {
		shorts = append(shorts, fmt.Sprint("s", i))
	}
	shortsURL := subscriptions.ChannelTabURL(chanID, subscriptions.TabShorts)
	sc.Playlists["shorts"] = scenario.Playlist{ID: chanID, Title: "Chan - Shorts", ChannelID: chanID, ExtractorKey: "YoutubeTab", Entries: shorts}
	sc.Aliases[shortsURL] = scenario.PlaylistPrefix + "shorts"
	slow := sc.Videos["v2"]
	slow.Download = []scenario.Behavior{{SleepMs: 400}}
	sc.Videos["v2"] = slow
	w := newWorld(t, sc)
	m := w.startSubs()
	ctx := context.Background()
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Mode: strp(store.ModeAuto), Sources: []SourceInput{{URL: chanVideos}}}))
	w.waitChecked(id, start)
	start = time.Now()
	if _, err := m.UpdateSubscription(ctx, id, SubscriptionInput{AddSources: []SourceInput{{URL: shortsURL}}}); err != nil {
		t.Fatal(err)
	}
	w.waitChecked(id, start)
	jobs := w.subJobs(id)
	if len(jobs) != 1 || jobs[0].Counts.Total != 1 || len(w.entries(id, store.EntryBacklog)) != 41 {
		t.Fatalf("added source: %d jobs, %d backlog", len(jobs), len(w.entries(id, store.EntryBacklog)))
	}
	waitItemState(t, m, jobs[0].ID, store.ItemDownloading)
	if err := m.DeleteSubscription(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	j := w.wait(jobs[0].ID, store.JobCompleted)
	if j.SubscriptionID != 0 || len(w.libFiles()) == 0 {
		t.Fatalf("job after delete: sub %d, files %v", j.SubscriptionID, w.libFiles())
	}
	if _, err := m.Store.GetSubscription(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("subscription not deleted")
	}
}

// §12: changed filters re-evaluate entries without queueing; a forced
// re-download shows the entry as queued.
func TestSubscriptionFiltersAndForce(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 3, newest(1, 1), newest(1, 3))
	slow := sc.Videos["v3"]
	slow.Download = []scenario.Behavior{{}, {SleepMs: 500}}
	sc.Videos["v3"] = slow
	w := newWorld(t, sc)
	m := w.startSubs()
	ctx := context.Background()
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Mode: strp(store.ModeAuto),
		Filters: &store.Filters{MinDuration: 600}, Sources: []SourceInput{{URL: chanVideos}}}))
	w.waitChecked(id, start)
	w.checkNow(id)
	// Filters apply to the backlog too: v1 (backlog), v2 and v3 (new).
	if n := len(w.entries(id, store.EntryIgnored)); n != 3 || len(w.subJobs(id)) != 0 {
		t.Fatalf("filtered: %d ignored, %d jobs", n, len(w.subJobs(id)))
	}
	if _, err := m.UpdateSubscription(ctx, id, SubscriptionInput{Filters: &store.Filters{}}); err != nil {
		t.Fatal(err)
	}
	if got := entryIDs(w.entries(id, "inbox")); got != "v3,v2" || len(w.subJobs(id)) != 0 {
		t.Fatalf("after clearing the filters: inbox %s, %d jobs", got, len(w.subJobs(id)))
	}
	res, _ := m.DownloadEntries(ctx, DownloadEntriesRequest{IDs: []string{w.entries(id, "inbox")[0].ID}})
	w.wait(mustID(res.Jobs[0].ID), store.JobCompleted)
	lib := w.entries(id, store.EntryLibrary)
	if res, _ := m.DownloadEntries(ctx, DownloadEntriesRequest{IDs: []string{lib[0].ID}}); res.Skipped != 1 {
		t.Fatal("an entry in the library was downloaded again without force")
	}
	res, err := m.DownloadEntries(ctx, DownloadEntriesRequest{IDs: []string{lib[0].ID}, Force: true})
	if err != nil || len(res.Jobs) != 1 || !res.Jobs[0].Options.Force {
		t.Fatalf("force: %+v %v", res, err)
	}
	if es := w.entries(id, store.EntryQueued); len(es) != 1 || es[0].Key != "fake v3" {
		t.Fatalf("a forced re-download shows as queued: %+v", es)
	}
	w.wait(mustID(res.Jobs[0].ID), store.JobCompleted)
	if es := w.entries(id, store.EntryLibrary); len(es) != 1 {
		t.Fatalf("after the forced download: %d in library", len(es))
	}
}

// §12: the backlog choice of the add dialog and Download backlog… create
// backlog-band jobs; a backlog job yields to a new auto job.
func TestSubscriptionBacklogChoice(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 8, newest(1, 6), newest(1, 8))
	for i := 1; i <= 6; i++ {
		v := sc.Videos[fmt.Sprint("v", i)]
		v.Download = []scenario.Behavior{{SleepMs: 150}}
		sc.Videos[v.ID] = v
	}
	w := newWorld(t, sc)
	w.startSubs()
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Mode: strp(store.ModeAuto),
		Backlog: &store.BacklogChoice{Mode: store.BacklogLatest, N: 4}, Sources: []SourceInput{{URL: chanVideos}}}))
	sub := w.waitChecked(id, start)
	jobs := w.subJobs(id)
	if len(jobs) != 1 || jobs[0].Band != store.BandBacklog || sub.Backlog.Pending() {
		t.Fatalf("backlog choice: %+v %+v", jobs, sub.Backlog)
	}
	if got := itemKeys(w.items(jobs[0].ID)); got != "v3,v4,v5,v6" {
		t.Fatalf("latest 4, oldest first: %s", got)
	}
	waitItemState(t, w.m, jobs[0].ID, store.ItemDownloading)
	w.checkNow(id) // v7, v8: an auto job; the backlog job yields to it
	jobs = w.subJobs(id)
	if len(jobs) != 2 || jobs[1].Band != store.BandAuto {
		t.Fatalf("auto job: %+v", jobs)
	}
	auto := w.wait(jobs[1].ID, store.JobCompleted)
	backlog := w.wait(jobs[0].ID, store.JobCompleted)
	if !auto.FinishedAt.Before(backlog.FinishedAt) || !strings.Contains(w.log(backlog.ID), "Giving the slot") {
		t.Fatalf("the backlog job did not yield: auto %v backlog %v\n%s", auto.FinishedAt, backlog.FinishedAt, w.log(backlog.ID))
	}
	res, err := w.m.DownloadEntries(context.Background(), DownloadEntriesRequest{Subscription: fmt.Sprint(id), Status: store.EntryBacklog})
	if err != nil || len(res.Jobs) != 1 || res.Queued != 2 || res.Jobs[0].Band != "backlog" {
		t.Fatalf("download backlog: %+v %v", res, err)
	}
}

// Edits and deletes during a check: the state at commit time applies.
func TestSubscriptionEditDuringCheck(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 2, newest(1, 1), newest(1, 2))
	pl := sc.Playlists["chan"]
	pl.Probe = []scenario.Behavior{{}, {SleepMs: 500}}
	sc.Playlists["chan"] = pl
	w := newWorld(t, sc)
	m := w.startSubs()
	ctx := context.Background()
	start := time.Now()
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Sources: []SourceInput{{URL: chanVideos}}}))
	w.waitChecked(id, start)
	start = time.Now()
	m.CheckSubscription(ctx, id, false)
	waitFor(t, func() bool { return m.subs.loop.Running(id) })
	if _, err := m.UpdateSubscription(ctx, id, SubscriptionInput{Mode: strp(store.ModeAuto)}); err != nil {
		t.Fatal(err)
	}
	w.waitChecked(id, start)
	if jobs := w.subJobs(id); len(jobs) != 1 {
		t.Fatalf("the new mode must apply at commit: %d jobs", len(jobs))
	}

	// Deleted during the check: the commit leaves nothing behind.
	m.CheckSubscription(ctx, id, false)
	waitFor(t, func() bool { return m.subs.loop.Running(id) })
	if err := m.DeleteSubscription(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !m.subs.loop.Running(id) })
	if n, _ := m.Store.CountEntries(ctx, id, store.StatusParams{}); n[id].Total != 0 {
		t.Fatalf("entries of the deleted subscription: %+v", n[id])
	}
	if b, _ := m.subs.logs.Read(id); len(b) != 0 {
		t.Fatalf("check log of the deleted subscription:\n%s", b)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// §11: one notification per check with new entries (notify), one when the
// subscription enters the error state (onFailed), not on every check.
func TestSubscriptionNotifications(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer hook.Close()
	sc := &scenario.Scenario{}
	channel(sc, 3, newest(1, 1), newest(1, 3))
	pl := sc.Playlists["chan"]
	pl.Probe = []scenario.Behavior{{}, {}, {Exit: 1, Stderr: []string{"ERROR: [youtube:tab] chan: This channel does not exist."}}}
	sc.Playlists["chan"] = pl
	w := newWorld(t, sc)
	m := w.startSubs()
	m.Settings.Update(context.Background(), func(s *config.Settings) error {
		s.Notifications = config.Notifications{WebhookURL: hook.URL, OnFailed: true}
		return nil
	})
	events := func(n int) []map[string]any {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			c := append([]map[string]any(nil), got...)
			mu.Unlock()
			if len(c) >= n || time.Now().After(deadline) {
				return c
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	start := time.Now()
	notifyOn := true
	id := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Notify: &notifyOn, Sources: []SourceInput{{URL: chanVideos}}}))
	w.waitChecked(id, start)
	w.checkNow(id)
	ev := events(1)
	if len(ev) != 1 || ev[0]["event"] != "subscription.new" || ev[0]["message"] != "Chan: 2 new videos" ||
		ev[0]["subscription"].(map[string]any)["count"] != 2.0 {
		t.Fatalf("new videos: %v", ev)
	}
	w.checkNow(id)
	w.checkNow(id) // still failing: no second message
	events(2)
	time.Sleep(200 * time.Millisecond)
	if ev = events(2); len(ev) != 2 || ev[1]["event"] != "subscription.error" {
		t.Fatalf("error notifications: %v", ev)
	}
}

// §11: import from a URL list, OPML and the YouTube CSV; duplicates and
// existing sources are skipped; the first checks are spread out; export
// writes the canonical URLs.
func TestSubscriptionImportExport(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 2, newest(1, 2))
	w := newWorld(t, sc)
	m := w.startSubs()
	ctx := context.Background()
	w.subscribe(SubscriptionInput{Name: strp("Existing"), Sources: []SourceInput{{URL: scenario.PlaylistPrefix + "other"}}})
	const other = "UCabcdefghijabcdefghij12"
	csvData := "Channel Id,Channel Url,Channel Title\n" + chanID + ",http://www.youtube.com/channel/" + chanID + ",Chan\n" +
		other + ",http://www.youtube.com/channel/" + other + ",Other\n"
	res, err := m.ImportSubscriptions(ctx, FormatYouTubeCSV, csvData)
	if err != nil || len(res.Created) != 2 || res.Created[0].Name != "Chan" || res.Created[0].Sources[0].Canonical != chanVideos {
		t.Fatalf("csv: %+v %v", res, err)
	}
	if res.Created[0].Mode != "manual" || res.Created[1].NextCheckAt.Before(res.Created[0].NextCheckAt.Add(20*time.Minute)) {
		t.Fatalf("imported subscriptions must be manual with spread first checks: %+v", res.Created)
	}
	opml := `<?xml version="1.0"?><opml version="1.1"><body><outline text="YouTube">
		<outline text="Chan again" xmlUrl="https://www.youtube.com/feeds/videos.xml?channel_id=` + chanID + `"/>
		<outline title="A list" htmlUrl="` + scenario.PlaylistPrefix + `pl"/></outline></body></opml>`
	res, err = m.ImportSubscriptions(ctx, FormatOPML, opml)
	if err != nil || len(res.Created) != 1 || len(res.Skipped) != 1 || res.Skipped[0].Reason != "already subscribed" {
		t.Fatalf("opml: %+v %v", res, err)
	}
	res, err = m.ImportSubscriptions(ctx, FormatURLs, "# comment\n"+scenario.PlaylistPrefix+"other/\nnot a url\nhttps://www.youtube.com/@someone\nhttps://www.youtube.com/@someone/videos\n")
	if err != nil || len(res.Created) != 1 || len(res.Skipped) != 3 {
		t.Fatalf("urls: %+v %v", res, err)
	}
	if v := res.Created[0]; v.Name != "https://www.youtube.com/@someone/videos" || v.Sources[0].Tab != "videos" {
		t.Fatalf("handle URL: %+v", v)
	}
	if _, err := m.ImportSubscriptions(ctx, "xls", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown format: %v", err)
	}
	// The imported channel without a title is named after its first listing.
	chanSub := int64(0)
	subs, _ := m.ListSubscriptions(ctx, "")
	for _, s := range subs {
		if s.Sources[0].Canonical == chanVideos {
			chanSub = sid(&s)
		}
	}
	m.Store.InTx(ctx, func(tx *store.Tx) error {
		sub, _ := tx.GetSubscription(ctx, chanSub)
		sub.Name = ""
		return tx.UpdateSubscription(ctx, sub)
	})
	if sub := w.checkNow(chanSub); sub.Name != "Chan" {
		t.Fatalf("name from the listing: %q", sub.Name)
	}

	urls, _ := m.ExportSubscriptions(ctx, FormatURLs)
	if !strings.Contains(string(urls), chanVideos+"\n") || strings.Count(string(urls), "\n") != 6 {
		t.Fatalf("url export:\n%s", urls)
	}
	out, _ := m.ExportSubscriptions(ctx, FormatOPML)
	if !strings.Contains(string(out), `xmlUrl="https://www.youtube.com/feeds/videos.xml?channel_id=`+chanID+`"`) {
		t.Fatalf("opml export:\n%s", out)
	}
	// The export imports again without creating anything.
	if res, _ = m.ImportSubscriptions(ctx, FormatOPML, string(out)); len(res.Created) != 0 || len(res.Skipped) != 5 {
		t.Fatalf("re-import: %+v", res)
	}
}

// §11: a handle URL of an imported list gets its canonical URL from the
// first listing; if that channel is subscribed already, the source is
// removed again and the subscription gets a warning (and is paused when
// nothing is left).
func TestSubscriptionImportedDuplicate(t *testing.T) {
	sc := &scenario.Scenario{}
	channel(sc, 2, newest(1, 2))
	// Checks list the provisional canonical URL of a handle.
	sc.Aliases["https://youtube.com/@chan/videos"] = scenario.PlaylistPrefix + "chan"
	w := newWorld(t, sc)
	m := w.startSubs()
	ctx := context.Background()
	start := time.Now()
	orig := sid(w.subscribe(SubscriptionInput{Name: strp("Chan"), Sources: []SourceInput{{URL: chanVideos}}}))
	w.waitChecked(orig, start)
	res, err := m.ImportSubscriptions(ctx, FormatURLs, "https://www.youtube.com/@chan\n")
	if err != nil || len(res.Created) != 1 {
		t.Fatalf("import: %+v %v", res, err)
	}
	dup := sid(&res.Created[0])
	sub := w.waitChecked(dup, start)
	if len(sub.Sources) != 0 || sub.Enabled || !strings.Contains(sub.Warning, `already subscribed in "Chan"`) {
		t.Fatalf("duplicate: %+v", sub)
	}
	if n := len(w.entries(dup, "")); n != 0 {
		t.Fatalf("the duplicate recorded %d entries", n)
	}
	if v, err := m.GetSubscription(ctx, dup); err != nil || v.Name != "https://www.youtube.com/@chan/videos" {
		t.Fatalf("view of the duplicate: %+v %v", v, err)
	}
}
