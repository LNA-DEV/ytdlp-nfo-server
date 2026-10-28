package store

import (
	"context"
	"testing"
	"time"
)

func TestMediaIndex(t *testing.T) {
	s := openTest(t, true)
	ctx := context.Background()
	sub := newSub(t, s, "r/pics", "https://www.reddit.com/r/pics/new")
	job := &Job{URL: "https://www.reddit.com/r/pics/new", State: JobRunning, SubscriptionID: sub.ID}
	s.CreateJob(ctx, job)
	gal := &Item{JobID: job.ID, State: ItemFinalizing, ArchiveKey: "redditpost g1"}
	dup := &Item{JobID: job.ID, Index: 1, State: ItemVerifying, ArchiveKey: "redditmedia new1"}
	s.CreateItems(ctx, []*Item{gal, dup})
	galEntry := addEntry(t, s, &Entry{SubscriptionID: sub.ID, Key: "redditpost g1", ItemID: gal.ID})
	cross := addEntry(t, s, &Entry{SubscriptionID: sub.ID, Key: "redditmedia a"})                       // the gallery's image, posted alone
	dupEntry := addEntry(t, s, &Entry{SubscriptionID: sub.ID, Key: "redditmedia new1", ItemID: dup.ID}) // re-upload of image b

	// A gallery of two images is finalized.
	gal.State, gal.ArchiveKey = ItemDone, "redditgallery h1"
	files := []MediaRow{{MediaKey: "redditmedia a", Path: "pics/G/G - 01.png", SHA256: "sa"},
		{MediaKey: "redditmedia b", Path: "pics/G/G - 02.png", SHA256: "sb"}}
	if err := s.FinalizeItem(ctx, gal, ArchiveEntry{Key: "redditgallery h1", Extractor: "redditgallery", VideoID: "h1",
		Path: "pics/G", FinalizedAt: time.Now()}, files...); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"redditgallery h1", "redditmedia a", "redditmedia b"} {
		if ok, _ := s.InLibrary(ctx, k); !ok {
			t.Fatalf("%s not in library", k)
		}
	}
	if m, _ := s.MediaBySHA(ctx, "sb"); m == nil || m.Path != "pics/G/G - 02.png" || m.ArchiveKey != "redditgallery h1" {
		t.Fatalf("by sha: %+v", m)
	}

	// An identical re-upload of image b is skipped with an alias.
	dup.State, dup.Error = ItemSkipped, "Identical to pics/G/G - 02.png"
	if err := s.SkipDuplicate(ctx, dup, []MediaRow{{MediaKey: "redditmedia new1", SHA256: "sb", Alias: true}}); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.MediaByKey(ctx, "redditmedia new1"); m == nil || !m.Alias || m.ArchiveKey != "redditgallery h1" || m.Path != "pics/G/G - 02.png" {
		t.Fatalf("alias: %+v", m)
	}
	// An alias whose target left the library writes nothing.
	if err := s.SkipDuplicate(ctx, dup, []MediaRow{{MediaKey: "redditmedia zzz", SHA256: "unknown", Alias: true},
		{MediaKey: "redditgallery gone", ArchiveKey: "redditgallery missing", Alias: true}}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.InLibrary(ctx, "redditmedia zzz"); ok {
		t.Fatal("alias without target")
	}
	if ok, _ := s.InLibrary(ctx, "redditgallery gone"); ok {
		t.Fatal("alias of a missing archive entry")
	}

	// Entries: the gallery (re-keyed), the crosspost of its image and the
	// duplicate are all in the library.
	got, _ := s.EntriesByID(ctx, []int64{galEntry.ID, cross.ID, dupEntry.ID}, StatusParams{})
	for _, e := range got {
		if e.Status != EntryLibrary || !e.InArchive {
			t.Fatalf("entry %s: %s", e.Key, e.Status)
		}
	}
	if got[0].Key != "redditgallery h1" || got[0].ListingKey != "redditpost g1" {
		t.Fatalf("gallery entry: %+v", got[0])
	}
	err := s.InTx(ctx, func(tx *Tx) error {
		in, err := tx.ArchivedKeys(ctx, []string{"redditmedia a", "redditmedia new1", "redditmedia other", "redditgallery h1"})
		if err != nil {
			return err
		}
		if len(in) != 3 || in["redditmedia other"] {
			t.Errorf("archived keys: %v", in)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A forced re-download replaces the files and keeps the aliases.
	if err := s.FinalizeItem(ctx, gal, ArchiveEntry{Key: "redditgallery h1", Extractor: "redditgallery", VideoID: "h1",
		Path: "pics/G (2)", FinalizedAt: time.Now()}, MediaRow{MediaKey: "redditmedia a", Path: "pics/G (2)/G - 01.png", SHA256: "sa"},
		MediaRow{MediaKey: "redditmedia b", Path: "pics/G (2)/G - 02.png", SHA256: "sb"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.MediaOf(ctx, "redditgallery h1")
	if len(rows) != 3 || rows[0].Alias != true || rows[1].Path != "pics/G (2)/G - 01.png" {
		t.Fatalf("after re-download: %+v", rows)
	}

	// Removing the archive entry removes its files and aliases.
	if err := s.RemoveArchive(ctx, "redditgallery h1"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"redditmedia a", "redditmedia new1"} {
		if ok, _ := s.InLibrary(ctx, k); ok {
			t.Fatalf("%s still in library", k)
		}
	}
	got, _ = s.EntriesByID(ctx, []int64{cross.ID}, StatusParams{})
	if got[0].Status == EntryLibrary {
		t.Fatal("crosspost entry still in library")
	}
}
