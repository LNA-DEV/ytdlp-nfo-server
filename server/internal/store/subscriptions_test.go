package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

func newSub(t *testing.T, s *Store, name string, canon ...string) *Subscription {
	t.Helper()
	sub := &Subscription{Name: name, Mode: ModeManual, Enabled: true}
	for _, c := range canon {
		sub.Sources = append(sub.Sources, &Source{URL: c, Canonical: c})
	}
	if err := s.CreateSubscription(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	return sub
}

func addEntry(t *testing.T, s *Store, e *Entry) *Entry {
	t.Helper()
	now := time.Now()
	if e.SortDate.IsZero() {
		e.SortDate = now
	}
	if e.FirstSeenAt.IsZero() {
		e.FirstSeenAt = now
	}
	e.LastSeenAt = now
	if e.URL == "" {
		e.URL = "https://fake.test/v/" + e.Key
	}
	if err := s.InTx(context.Background(), func(tx *Tx) error { return tx.InsertEntry(context.Background(), e) }); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSubscriptionCRUDAndConflicts(t *testing.T) {
	s := openTest(t, true)
	ctx := context.Background()
	a := newSub(t, s, "A", "https://fake.test/pl/a", "https://fake.test/pl/a2")
	if len(a.Sources) != 2 || a.Sources[0].ID == 0 {
		t.Fatalf("sources not created: %+v", a.Sources)
	}
	b := &Subscription{Name: "B", Mode: ModeAuto, Enabled: true, Sources: []*Source{{URL: "x", Canonical: "https://fake.test/pl/a2"}}}
	var conflict *SourceConflictError
	if err := s.CreateSubscription(ctx, b); !errors.As(err, &conflict) || conflict.SubscriptionID != a.ID {
		t.Fatalf("want conflict with %d, got %v", a.ID, err)
	}
	if subs, _ := s.ListSubscriptions(ctx); len(subs) != 1 {
		t.Fatalf("failed create left a subscription behind: %d", len(subs))
	}
	got, err := s.GetSubscription(ctx, a.ID)
	if err != nil || got.Name != "A" || len(got.Sources) != 2 || got.State != SubIdle || got.Listing != ListingAuto {
		t.Fatalf("get: %+v %v", got, err)
	}
	got.Folder, got.Mode = "Folder", ModeAuto
	got.Filters.TitleExclude = "trailer"
	got.Backlog = BacklogChoice{Mode: BacklogLatest, N: 3}
	if err := s.UpdateSubscription(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSubscription(ctx, a.ID)
	if got.EffectiveFolder() != "Folder" || got.Mode != ModeAuto || got.Filters.TitleExclude != "trailer" || !got.Backlog.Pending() {
		t.Fatalf("update not persisted: %+v", got)
	}
	srcs, _ := s.SourcesByCanonical(ctx, []string{"https://fake.test/pl/a2", "nope"})
	if len(srcs) != 1 || srcs[0].SubscriptionID != a.ID {
		t.Fatalf("by canonical: %+v", srcs)
	}

	// Due subscriptions and crash reset.
	now := time.Now()
	if n, _ := s.SetNextCheck(ctx, nil, now.Add(-time.Second)); n != 1 {
		t.Fatalf("set next check: %d", n)
	}
	if due, _ := s.DueSubscriptions(ctx, now); len(due) != 1 {
		t.Fatalf("due: %d", len(due))
	}
	s.SetSubscriptionState(ctx, a.ID, SubChecking)
	if due, _ := s.DueSubscriptions(ctx, now); len(due) != 0 {
		t.Fatal("a subscription being checked must not be due")
	}
	if n, _ := s.ResetChecking(ctx, now); n != 1 {
		t.Fatalf("reset: %d", n)
	}

	// A full rescan request is only cleared by the check that did it.
	if err := s.RequestFullCheck(ctx, a.ID, now); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetSubscription(ctx, a.ID)
	got.Failures = 1
	if err := s.UpdateSubscription(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := s.InTx(ctx, func(tx *Tx) error { return tx.SaveCheckState(ctx, got) }); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetSubscription(ctx, a.ID); !got.FullRequested {
		t.Fatal("full rescan request lost by an update")
	}
	if err := s.InTx(ctx, func(tx *Tx) error { return tx.ClearFullRequest(ctx, a.ID) }); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetSubscription(ctx, a.ID); got.FullRequested {
		t.Fatal("full rescan request not cleared")
	}

	// Deleting a subscription keeps its jobs (subscription_id → NULL) and
	// a later UpdateJob does not restore the stale reference.
	j := &Job{URL: "https://fake.test/pl/a", State: JobRunning, SubscriptionID: a.ID, Band: BandAuto}
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.SubscriptionJobIDs(ctx, a.ID); len(ids) != 1 {
		t.Fatalf("subscription jobs: %v", ids)
	}
	if err := s.DeleteSubscription(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateJob(ctx, j); err != nil {
		t.Fatalf("update after delete: %v", err)
	}
	j2, _ := s.GetJob(ctx, j.ID)
	if j2.SubscriptionID != 0 || j2.Band != BandAuto {
		t.Fatalf("job after delete: sub=%d band=%v", j2.SubscriptionID, j2.Band)
	}
	if srcs, _ := s.SourcesByCanonical(ctx, []string{"https://fake.test/pl/a"}); len(srcs) != 0 {
		t.Fatal("sources not deleted")
	}
	// The canonical URL is free again.
	newSub(t, s, "A again", "https://fake.test/pl/a")
}

func TestDerivedEntryStatus(t *testing.T) {
	s := openTest(t, false)
	ctx := context.Background()
	sub := newSub(t, s, "S", "https://fake.test/pl/s")
	job := &Job{URL: "https://fake.test/pl/s", State: JobRunning, SubscriptionID: sub.ID, Band: BandAuto}
	s.CreateJob(ctx, job)
	item := func(state ItemState, errText string) int64 {
		it := &Item{JobID: job.ID, State: state, Error: errText}
		if err := s.CreateItems(ctx, []*Item{it}); err != nil {
			t.Fatal(err)
		}
		return it.ID
	}
	s.ImportArchiveKeys(ctx, []string{"fake lib", "fake forced"}, "test")
	old := time.Now().Add(-30 * 24 * time.Hour)
	cases := []struct {
		e    *Entry
		want string
	}{
		{&Entry{Key: "fake queued", ItemID: item(ItemDownloading, "")}, EntryQueued},
		{&Entry{Key: "fake forced", ItemID: item(ItemPending, "")}, EntryQueued}, // forced re-download
		{&Entry{Key: "fake lib", Ignored: true}, EntryLibrary},
		{&Entry{Key: "fake ign", Ignored: true, LiveStatus: "is_upcoming"}, EntryIgnored},
		{&Entry{Key: "fake live", LiveStatus: "post_live", Backlog: true}, EntryUpcoming},
		{&Entry{Key: "fake nya", ItemID: item(ItemFailed, ytdlp.ReasonNotYetAvailable)}, EntryUpcoming},
		{&Entry{Key: "fake nya-old", FirstSeenAt: old, ItemID: item(ItemFailed, ytdlp.ReasonNotYetAvailable)}, EntryFailed},
		{&Entry{Key: "fake failed", ItemID: item(ItemFailed, "video is private")}, EntryFailed},
		{&Entry{Key: "fake cancelled", ItemID: item(ItemCancelled, "")}, EntryFailed},
		{&Entry{Key: "fake skipped", ItemID: item(ItemSkipped, "")}, EntryNew},
		{&Entry{Key: "fake new"}, EntryNew},
		{&Entry{Key: "fake backlog", Backlog: true}, EntryBacklog},
	}
	p := StatusParams{UpcomingSince: time.Now().Add(-7 * 24 * time.Hour)}
	var ids []int64
	for i, c := range cases {
		c.e.SubscriptionID = sub.ID
		c.e.SortDate = time.Now().Add(-time.Duration(i) * time.Hour)
		addEntry(t, s, c.e)
		ids = append(ids, c.e.ID)
	}
	got, err := s.EntriesByID(ctx, ids, p)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range got {
		if e.Status != cases[i].want {
			t.Errorf("%s: status %s, want %s", e.Key, e.Status, cases[i].want)
		}
	}
	if !got[0].ItemActive() || got[0].JobID != job.ID || !got[5].NotYetAvailable() || got[2].InArchive != true {
		t.Fatalf("derived fields: %+v %+v", got[0], got[5])
	}

	counts, err := s.CountEntries(ctx, 0, p)
	if err != nil {
		t.Fatal(err)
	}
	c := counts[sub.ID]
	if c.Newest.IsZero() {
		t.Fatal("newest entry date missing")
	}
	c.Newest = time.Time{}
	want := EntryCounts{Total: 12, New: 2, Backlog: 1, Library: 1, Ignored: 1, Failed: 3, Queued: 2, Upcoming: 2,
		NewTab: 5, BacklogTab: 2}
	if c != want {
		t.Fatalf("counts %+v, want %+v", c, want)
	}

	list := func(status string) []string {
		es, err := s.ListEntries(ctx, EntryFilter{SubscriptionID: sub.ID, Status: status}, p)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, e := range es {
			keys = append(keys, e.Key)
		}
		return keys
	}
	if got := fmt.Sprint(list("new")); got != "[fake queued fake forced fake nya fake skipped fake new]" {
		t.Fatalf("new tab: %s", got)
	}
	if got := fmt.Sprint(list("backlog")); got != "[fake live fake backlog]" {
		t.Fatalf("backlog tab: %s", got)
	}
	if got := fmt.Sprint(list("inbox")); got != "[fake skipped fake new]" {
		t.Fatalf("inbox: %s", got)
	}
	if got := len(list("")); got != 12 {
		t.Fatalf("all: %d", got)
	}
	es, _ := s.ListEntries(ctx, EntryFilter{Query: "nya", Limit: 1, Offset: 1}, p)
	if len(es) != 1 || es[0].Key != "fake nya-old" {
		t.Fatalf("query/offset: %+v", es)
	}

	// Deleting the job unlinks the items: entries fall back.
	if err := s.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.EntriesByID(ctx, ids[:2], p)
	if got[0].ItemID != 0 || got[0].Status != EntryNew || got[1].Status != EntryLibrary {
		t.Fatalf("after job delete: %+v %+v", got[0], got[1])
	}

	n, err := s.SetIgnored(ctx, []int64{ids[0]}, true)
	if err != nil || n != 1 {
		t.Fatal(err)
	}
	got, _ = s.EntriesByID(ctx, ids[:1], p)
	if got[0].Status != EntryIgnored {
		t.Fatalf("ignore: %s", got[0].Status)
	}
}

func TestFinalizeRekeysAndMergesEntries(t *testing.T) {
	s := openTest(t, true)
	ctx := context.Background()
	a := newSub(t, s, "A", "https://fake.test/pl/a", "https://fake.test/pl/a-live")
	b := newSub(t, s, "B", "https://fake.test/pl/b")
	job := &Job{URL: "https://fake.test/pl/a", State: JobRunning, SubscriptionID: a.ID}
	s.CreateJob(ctx, job)
	it := &Item{JobID: job.ID, State: ItemFinalizing, ArchiveKey: "url https://fake.test/v/x"}
	s.CreateItems(ctx, []*Item{it})
	// A has the video twice (two sources listed it differently), B once
	// under the old key.
	old := addEntry(t, s, &Entry{SubscriptionID: a.ID, Key: "url https://fake.test/v/x", ItemID: it.ID})
	dup := addEntry(t, s, &Entry{SubscriptionID: a.ID, Key: "fake x"})
	other := addEntry(t, s, &Entry{SubscriptionID: b.ID, Key: "url https://fake.test/v/x"})

	it.State, it.ArchiveKey = ItemDone, "fake x"
	if err := s.FinalizeItem(ctx, it, ArchiveEntry{Key: "fake x", Extractor: "fake", VideoID: "x", FinalizedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p := StatusParams{}
	got, _ := s.EntriesByID(ctx, []int64{old.ID, dup.ID, other.ID}, p)
	if len(got) != 2 {
		t.Fatalf("want the duplicate merged, got %d entries", len(got))
	}
	if got[0].ID != dup.ID || got[0].ItemID != it.ID || got[0].Status != EntryLibrary {
		t.Fatalf("merged entry: %+v", got[0])
	}
	if got[1].ID != other.ID || got[1].Key != "fake x" || got[1].Status != EntryLibrary {
		t.Fatalf("other subscription: %+v", got[1])
	}
	// Listings keep reporting the old key: both entries remember it.
	if got[0].ListingKey != "url https://fake.test/v/x" || got[1].ListingKey != "url https://fake.test/v/x" {
		t.Fatalf("listing keys: %q %q", got[0].ListingKey, got[1].ListingKey)
	}
	keys, _ := s.EntryKeys(ctx, b.ID)
	if !keys["fake x"] || !keys["url https://fake.test/v/x"] {
		t.Fatalf("entry keys: %v", keys)
	}
	if ok, _ := s.HasArchive(ctx, "fake x"); !ok {
		t.Fatal("archive entry missing")
	}
}

// An item that turns out to be in the library already re-keys its entries
// too, so they show as in library.
func TestRekeyEntriesOfSkippedItem(t *testing.T) {
	s := openTest(t, true)
	ctx := context.Background()
	a := newSub(t, s, "A", "https://fake.test/pl/a")
	job := &Job{URL: "https://fake.test/pl/a", State: JobRunning, SubscriptionID: a.ID}
	s.CreateJob(ctx, job)
	it := &Item{JobID: job.ID, State: ItemSkipped, ArchiveKey: "fake y"}
	s.CreateItems(ctx, []*Item{it})
	e := addEntry(t, s, &Entry{SubscriptionID: a.ID, Key: "url https://fake.test/v/y", ItemID: it.ID})
	s.ImportArchiveKeys(ctx, []string{"fake y"}, "test")
	if err := s.RekeyEntries(ctx, it.ID, "fake y"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.EntriesByID(ctx, []int64{e.ID}, StatusParams{})
	if got[0].Key != "fake y" || got[0].ListingKey != "url https://fake.test/v/y" || got[0].Status != EntryLibrary {
		t.Fatalf("entry: %+v", got[0])
	}
}

// BenchmarkEntryPages checks the §12 target: one page of 50 in under
// 100 ms with 200 000 entries, for every list filter of the feed and of a
// subscription (rare statuses are the worst case: the page has fewer than
// 50 matches, so the whole candidate range is walked).
func BenchmarkEntryPages(b *testing.B) {
	s, err := Open(b.TempDir() + "/bench.db")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	const subs, per = 20, 10000
	var keys []string
	var firstSub int64
	now := time.Now()
	for i := 0; i < subs; i++ {
		sub := &Subscription{Name: fmt.Sprint("S", i), Mode: ModeManual, Enabled: true,
			Sources: []*Source{{URL: "u", Canonical: fmt.Sprint("https://fake.test/pl/", i)}}}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			b.Fatal(err)
		}
		if firstSub == 0 {
			firstSub = sub.ID
		}
		job := &Job{URL: "u", State: JobPartial, SubscriptionID: sub.ID}
		s.CreateJob(ctx, job)
		err := s.InTx(ctx, func(tx *Tx) error {
			for j := 0; j < per; j++ {
				k := fmt.Sprintf("fake %d-%d", i, j)
				e := &Entry{SubscriptionID: sub.ID, Key: k, URL: "https://fake.test/v/" + k, Title: k, Backlog: true,
					SortDate: now.Add(-time.Duration(j*subs+i) * time.Minute), FirstSeenAt: now, LastSeenAt: now,
					LiveStatus: "not_live"}
				switch {
				case j%2000 == 0:
					e.Backlog = false // a few new entries, spread over the whole range
				case j%2000 == 1:
					e.Ignored = true
				case j%2000 == 2:
					e.LiveStatus = "is_upcoming"
				case j%500 == 3:
					it := &Item{JobID: job.ID, State: ItemFailed, Error: "video is private"}
					if err := tx.CreateItems(ctx, []*Item{it}); err != nil {
						return err
					}
					e.ItemID = it.ID
				}
				if err := tx.InsertEntry(ctx, e); err != nil {
					return err
				}
				if j%3 == 0 {
					keys = append(keys, k)
				}
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	if _, err := s.ImportArchiveKeys(ctx, keys, "bench"); err != nil {
		b.Fatal(err)
	}
	p := StatusParams{UpcomingSince: now.Add(-7 * 24 * time.Hour)}
	for _, sub := range []int64{0, firstSub} {
		for _, st := range []string{"", "inbox", EntryNew, EntryBacklog, EntryLibrary, EntryIgnored, EntryQueued, EntryUpcoming, EntryFailed} {
			b.Run(fmt.Sprintf("sub=%d/%s", sub, st), func(b *testing.B) {
				var worst time.Duration
				for i := 0; i < b.N; i++ {
					for _, off := range []int{0, 100} {
						start := time.Now()
						if _, err := s.ListEntries(ctx, EntryFilter{SubscriptionID: sub, Status: st, Offset: off}, p); err != nil {
							b.Fatal(err)
						}
						worst = max(worst, time.Since(start))
					}
				}
				b.ReportMetric(float64(worst.Microseconds())/1000, "worst-ms")
			})
		}
	}
	b.Run("counts", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			c, err := s.CountEntries(ctx, 0, p)
			if err != nil || c[firstSub].Total != per {
				b.Fatal(err, c[firstSub])
			}
		}
	})
}

func TestMigrationFromVersion1(t *testing.T) {
	path := t.TempDir() + "/v1.db"
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, q := range []string{migrations[0], `CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version(version) VALUES (1)`,
		`INSERT INTO jobs (url, state, created_at, updated_at) VALUES ('https://fake.test/v/1', 'completed', 1, 1)`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j, err := s.GetJob(ctx, 1)
	if err != nil || j.SubscriptionID != 0 || j.Band != BandInteractive {
		t.Fatalf("job after migration: %+v %v", j, err)
	}
	newSub(t, s, "after migration", "https://fake.test/pl/m")
}

func TestPruneGoneEntries(t *testing.T) {
	s := openTest(t, false)
	ctx := context.Background()
	sub := newSub(t, s, "S", "https://fake.test/pl/s")
	old := time.Now().Add(-48 * time.Hour)
	s.ImportArchiveKeys(ctx, []string{"fake kept-archived"}, "test")
	gone := addEntry(t, s, &Entry{SubscriptionID: sub.ID, Key: "fake gone", GoneAt: old})
	addEntry(t, s, &Entry{SubscriptionID: sub.ID, Key: "fake kept-archived", GoneAt: old})
	addEntry(t, s, &Entry{SubscriptionID: sub.ID, Key: "fake kept-recent", GoneAt: time.Now()})
	addEntry(t, s, &Entry{SubscriptionID: sub.ID, Key: "fake listed"})
	n, err := s.PruneGoneEntries(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d: %v", n, err)
	}
	keys, _ := s.EntryKeys(ctx, sub.ID)
	if len(keys) != 3 || keys[gone.Key] {
		t.Fatalf("keys after prune: %v", keys)
	}
}
