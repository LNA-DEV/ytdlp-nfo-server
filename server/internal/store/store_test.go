package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
)

func openTest(t *testing.T, file bool) *Store {
	t.Helper()
	path := ""
	if file {
		path = filepath.Join(t.TempDir(), "test.db")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestJobLifecycle(t *testing.T) {
	for _, file := range []bool{false, true} {
		s := openTest(t, file)
		ctx := context.Background()
		j := &Job{URL: "https://example.com/v/1", NormURL: "https://example.com/v/1", State: JobQueued}
		if err := s.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		if j.ID == 0 || j.QueuePos != 1 {
			t.Fatalf("id=%d pos=%d", j.ID, j.QueuePos)
		}
		items := []*Item{
			{JobID: j.ID, Index: 0, State: ItemDone, ArchiveKey: "fake a"},
			{JobID: j.ID, Index: 1, State: ItemSkipped},
			{JobID: j.ID, Index: 2, State: ItemFailed, Warnings: []string{"No thumbnail"}},
			{JobID: j.ID, Index: 3, State: ItemPending},
		}
		if err := s.CreateItems(ctx, items); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetJob(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := Counts{Total: 4, Done: 1, Skipped: 1, Failed: 1, Pending: 1, Warnings: 1}
		if got.Counts != want {
			t.Fatalf("counts = %+v, want %+v", got.Counts, want)
		}
		got.State = JobRunning
		got.Retry.LastClass = "transient"
		got.RetryAt = time.Now().Add(time.Minute)
		if err := s.UpdateJob(ctx, got); err != nil {
			t.Fatal(err)
		}
		again, _ := s.GetJob(ctx, j.ID)
		if again.State != JobRunning || again.Retry.LastClass != "transient" || again.RetryAt.IsZero() {
			t.Fatalf("update not persisted: %+v", again)
		}
		counts, _ := s.CountJobsByTab(ctx)
		if counts["active"] != 1 {
			t.Fatalf("tab counts %v", counts)
		}
		if _, err := s.FindOpenJobByNormURL(ctx, j.NormURL); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteJob(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
		its, _ := s.ItemsForJob(ctx, j.ID)
		if len(its) != 0 {
			t.Fatalf("items not cascaded: %d", len(its))
		}
	}
}

func TestFinalizeItemTransaction(t *testing.T) {
	s := openTest(t, true)
	ctx := context.Background()
	j := &Job{URL: "u", State: JobRunning}
	s.CreateJob(ctx, j)
	it := &Item{JobID: j.ID, State: ItemFinalizing, ArchiveKey: "youtube abc"}
	s.CreateItems(ctx, []*Item{it})
	it.State = ItemDone
	it.FinalPath = "A [abc]"
	if err := s.FinalizeItem(ctx, it, ArchiveEntry{Key: "youtube abc", Extractor: "youtube", VideoID: "abc", Path: "A [abc]", JobID: j.ID, FinalizedAt: time.Now(), Source: "server"}); err != nil {
		t.Fatal(err)
	}
	ok, _ := s.HasArchive(ctx, "youtube abc")
	if !ok {
		t.Fatal("archive entry missing")
	}
	got, _ := s.GetItem(ctx, it.ID)
	if got.State != ItemDone || got.FinalPath != "A [abc]" {
		t.Fatalf("item = %+v", got)
	}
	// A failing item update must roll back the archive insert.
	bad := &Item{ID: 9999, State: ItemDone}
	if err := s.FinalizeItem(ctx, bad, ArchiveEntry{Key: "youtube zzz", Extractor: "youtube", VideoID: "zzz", FinalizedAt: time.Now()}); err == nil {
		t.Fatal("expected error")
	}
	if ok, _ := s.HasArchive(ctx, "youtube zzz"); ok {
		t.Fatal("archive entry written without item update")
	}
}

func TestQueueOrderAndListing(t *testing.T) {
	s := openTest(t, false)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 3; i++ {
		j := &Job{URL: "u", State: JobQueued, Title: []string{"alpha", "beta", "gamma"}[i]}
		s.CreateJob(ctx, j)
		ids = append(ids, j.ID)
	}
	if err := s.SetQueuePositions(ctx, []int64{ids[2]}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.ListJobs(ctx, JobFilter{States: TabStates("queued")})
	if jobs[0].ID != ids[2] || jobs[1].ID != ids[0] {
		t.Fatalf("order = %d,%d,%d", jobs[0].ID, jobs[1].ID, jobs[2].ID)
	}
	jobs, _ = s.ListJobs(ctx, JobFilter{Query: "bet"})
	if len(jobs) != 1 || jobs[0].Title != "beta" {
		t.Fatalf("search returned %d", len(jobs))
	}
	jobs, _ = s.ListJobs(ctx, JobFilter{Limit: 2, Offset: 2})
	if len(jobs) != 1 {
		t.Fatalf("pagination returned %d", len(jobs))
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := openTest(t, false)
	ctx := context.Background()
	if _, ok, _ := s.LoadSettings(ctx); ok {
		t.Fatal("unexpected settings")
	}
	st := config.DefaultSettings(true)
	st.Locked = []string{"maxConcurrent"}
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadSettings(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.Naming.Preset != config.NamingLegacy || got.Locked != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestV1Import(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	os.WriteFile(path, []byte(`{"nextId":42,"jobs":[
		{"id":"40","url":"https://example.com/a","status":"completed","createdAt":"2026-01-01T10:00:00Z","doneAt":"2026-01-01T10:05:00Z","options":{"format":"mp4","allAudio":false,"subtitles":true}},
		{"id":"41","url":"https://example.com/b","status":"failed","error":"exit status 1","createdAt":"2026-01-02T10:00:00Z","options":{}},
		{"id":"42","url":"https://example.com/c","status":"retrying","retryCount":2,"createdAt":"2026-01-03T10:00:00Z","options":{"format":"mkv","allAudio":true,"subtitles":true}}
	]}`), 0o644)
	st, err := ReadV1State(path)
	if err != nil {
		t.Fatal(err)
	}
	s := openTest(t, true)
	ctx := context.Background()
	var jobs []*Job
	for _, v := range st.Jobs {
		j, err := v.ToJob()
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	if err := s.CreateJobs(ctx, jobs); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNextJobID(ctx, int64(st.NextID)); err != nil {
		t.Fatal(err)
	}
	a, _ := s.GetJob(ctx, 40)
	if a.State != JobCompleted || *a.Requested.Container != "mp4" || a.FinishedAt.IsZero() {
		t.Fatalf("job 40 = %+v", a)
	}
	b, _ := s.GetJob(ctx, 41)
	if b.State != JobFailed || b.Error != "exit status 1" || b.Requested.Container != nil {
		t.Fatalf("job 41 = %+v", b)
	}
	c, _ := s.GetJob(ctx, 42)
	if c.State != JobQueued || c.Attempts != 0 {
		t.Fatalf("job 42 = %+v", c)
	}
	n := &Job{URL: "new", State: JobQueued}
	s.CreateJob(ctx, n)
	if n.ID != 43 {
		t.Fatalf("next id = %d, want 43", n.ID)
	}
}

func TestArchiveImportSearchRemove(t *testing.T) {
	s := openTest(t, false)
	ctx := context.Background()
	keys, _ := ParseArchive(strings.NewReader("youtube aaa\n\nbroken\nvimeo 123 \nyoutube aaa\n"))
	if len(keys) != 3 {
		t.Fatalf("keys = %v", keys)
	}
	n, err := s.ImportArchiveKeys(ctx, keys, "v1")
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	n, _ = s.ImportArchiveKeys(ctx, keys, "v1")
	if n != 0 {
		t.Fatalf("re-import added %d", n)
	}
	got, total, _ := s.SearchArchive(ctx, "vimeo", 10, 0)
	if total != 1 || got[0].VideoID != "123" {
		t.Fatalf("search = %v", got)
	}
	if err := s.RemoveArchive(ctx, "vimeo 123"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasArchive(ctx, "vimeo 123"); ok {
		t.Fatal("still present")
	}
}

func TestSessionsAndTokens(t *testing.T) {
	s := openTest(t, false)
	ctx := context.Background()
	s.CreateSession(ctx, "h1", time.Now().Add(time.Hour))
	s.CreateSession(ctx, "h2", time.Now().Add(-time.Hour))
	if ok, _ := s.SessionValid(ctx, "h1", time.Now().Add(2*time.Hour)); !ok {
		t.Fatal("h1 should be valid")
	}
	if ok, _ := s.SessionValid(ctx, "h2", time.Now().Add(2*time.Hour)); ok {
		t.Fatal("h2 expired")
	}
	tok := &APIToken{Name: "addon", Hash: "abc", Prefix: "ytn_ab"}
	s.CreateToken(ctx, tok)
	got, err := s.TokenByHash(ctx, "abc")
	if err != nil || got.Name != "addon" {
		t.Fatal(err)
	}
	list, _ := s.ListTokens(ctx)
	if len(list) != 1 || list[0].LastUsedAt.IsZero() {
		t.Fatalf("tokens = %+v", list)
	}
}
