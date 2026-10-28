package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/scenario"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp/versions"
)

var fakeDir string

func TestMain(m *testing.M) {
	dir, err := testutil.FakeBin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "building fake yt-dlp:", err)
		os.Exit(1)
	}
	fakeDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fakeResolver struct {
	stable, nightly versions.Binary
}

func (f *fakeResolver) Acquire(_ context.Context, spec string) (versions.Binary, func(), error) {
	if strings.HasPrefix(spec, "nightly") {
		return f.nightly, func() {}, nil
	}
	return f.stable, func() {}, nil
}

func (f *fakeResolver) IsLatest(_ context.Context, b versions.Binary) bool {
	return b.ID == f.nightly.ID
}

type world struct {
	t        *testing.T
	m        *Manager
	dir      string
	env      *config.Env
	sc       *scenario.Scenario
	resolver *fakeResolver
	state    string
}

func newWorld(t *testing.T, sc *scenario.Scenario) *world {
	t.Helper()
	dir := t.TempDir()
	w := &world{t: t, dir: dir, sc: sc}
	w.env = &config.Env{
		DataDir: filepath.Join(dir, "data"), DownloadDir: filepath.Join(dir, "downloads"), OutputDir: filepath.Join(dir, "media"),
		FFmpegPath: filepath.Join(fakeDir, "ffmpeg"), FFprobePath: filepath.Join(fakeDir, "ffprobe"), DenoPath: "deno-not-installed",
	}
	w.state = filepath.Join(dir, "state")
	os.MkdirAll(w.state, 0o755)
	scPath := filepath.Join(dir, "scenario.json")
	if sc.Version == "" {
		sc.Version = "2026.01.01"
	}
	if sc.Targets == nil {
		sc.Targets = []string{"Chrome-131 Windows-10"}
	}
	if err := sc.Write(scPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv(scenario.EnvScenario, scPath)
	t.Setenv(scenario.EnvState, w.state)
	nightly, err := testutil.NightlyCopy(filepath.Join(dir, "nightly"), "2026.10.10.1")
	if err != nil {
		t.Fatal(err)
	}
	w.resolver = &fakeResolver{
		stable:  versions.Binary{ID: "stable/2026.01.01", Channel: "stable", Tag: "2026.01.01", Version: "2026.01.01", Path: filepath.Join(fakeDir, "yt-dlp"), Targets: []string{"chrome-131:windows-10"}, ImpersonateAvailable: true},
		nightly: versions.Binary{ID: "nightly/2026.10.10.1", Channel: "nightly", Tag: "2026.10.10.1", Version: "2026.10.10.1", Path: nightly, Targets: []string{"chrome-131:windows-10"}, ImpersonateAvailable: true},
	}
	return w
}

func (w *world) start() *Manager {
	w.t.Helper()
	m, err := New(Options{
		Env: w.env, Resolver: w.resolver, DisableBackground: true, Logf: w.t.Logf,
		TweakPolicy: func(p *scheduler.Policy) {
			p.Base, p.Cap, p.RateLimitCooldown = 20*time.Millisecond, 100*time.Millisecond, 150*time.Millisecond
		},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := m.Settings.Update(context.Background(), func(s *config.Settings) error { s.MinFreeSpaceMB = 0; return nil }); err != nil {
		w.t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		w.t.Fatal(err)
	}
	w.m = m
	w.t.Cleanup(func() { w.stop() })
	return m
}

func (w *world) stop() {
	if w.m != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		w.m.Shutdown(ctx)
		w.m = nil
	}
}

func (w *world) submit(url string, req config.OptionsRequest) *store.Job {
	w.t.Helper()
	j, err := w.m.Submit(context.Background(), url, req)
	if err != nil {
		w.t.Fatalf("submit %s: %v", url, err)
	}
	return j
}

func (w *world) wait(id int64, states ...store.JobState) *store.Job {
	w.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		j, err := w.m.Store.GetJob(context.Background(), id)
		if err == nil {
			for _, s := range states {
				if j.State == s {
					return j
				}
			}
		}
		if time.Now().After(deadline) {
			b, _ := w.m.Logs.Read(id)
			w.t.Fatalf("job %d did not reach %v (state %v)\nlog:\n%s", id, states, j.State, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (w *world) items(id int64) []*store.Item {
	its, err := w.m.Store.ItemsForJob(context.Background(), id)
	if err != nil {
		w.t.Fatal(err)
	}
	return its
}

func (w *world) log(id int64) string {
	b, _ := w.m.Logs.Read(id)
	return string(b)
}

func (w *world) calls(key string) int {
	b, err := os.ReadFile(filepath.Join(w.state, key))
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscanf(string(b), "%d", &n)
	return n
}

func (w *world) libFiles() []string {
	var out []string
	root := w.env.LibraryRoot()
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") && rel != "." {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			out = append(out, rel)
		}
		return nil
	})
	return out
}

func vid(id, title string) scenario.Video {
	return scenario.Video{ID: id, Title: title, Duration: 120, Uploader: "Uploader"}
}

// --- §15 required regression scenarios ---

// All items already in the library: job completed at once, no retry (F2).
func TestScenarioAllInLibrary(t *testing.T) {
	sc := &scenario.Scenario{
		Videos:    map[string]scenario.Video{"a": vid("a", "A"), "b": vid("b", "B")},
		Playlists: map[string]scenario.Playlist{"p": {ID: "p", Title: "List", Entries: []string{"a", "b"}}},
	}
	w := newWorld(t, sc)
	m := w.start()
	m.Store.ImportArchiveKeys(context.Background(), []string{"fake a", "fake b"}, "test")
	j := w.submit(scenario.PlaylistPrefix+"p", config.OptionsRequest{})
	j = w.wait(j.ID, store.JobCompleted, store.JobFailed)
	if j.State != store.JobCompleted || j.Hint != "All 2 items already in library" || j.Attempts != 0 {
		t.Fatalf("job = %s hint=%q attempts=%d", j.State, j.Hint, j.Attempts)
	}
	for _, it := range w.items(j.ID) {
		if it.State != store.ItemSkipped {
			t.Fatalf("item %s = %s", it.VideoID, it.State)
		}
	}
	if w.calls("download-a")+w.calls("meta-a") != 0 {
		t.Fatal("archived item was processed")
	}
}

// Deterministic error: permanent fails after the first attempt; an unknown
// error repeated is stopped by the guard after the second (F15).
func TestScenarioDeterministicErrors(t *testing.T) {
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{
		"priv": {ID: "priv", Title: "P", Probe: []scenario.Behavior{{Exit: 1, Stderr: []string{"ERROR: [fake] priv: Private video. Sign in if you've been granted access to this video"}}}},
		"odd":  {ID: "odd", Title: "O", Probe: []scenario.Behavior{{Exit: 1, Stderr: []string{"ERROR: [fake] odd: Something nobody has seen before"}}}},
	}}
	w := newWorld(t, sc)
	w.start()
	j1 := w.submit(scenario.VideoPrefix+"priv", config.OptionsRequest{})
	j2 := w.submit(scenario.VideoPrefix+"odd", config.OptionsRequest{})
	j1 = w.wait(j1.ID, store.JobFailed)
	j2 = w.wait(j2.ID, store.JobFailed)
	if w.calls("probe-priv") != 1 || j1.ErrorClass != "permanent" || !strings.Contains(j1.Hint, "does not go away") {
		t.Fatalf("permanent: calls=%d class=%s hint=%q", w.calls("probe-priv"), j1.ErrorClass, j1.Hint)
	}
	if w.calls("probe-odd") != 2 || j2.ErrorClass != "unknown" {
		t.Fatalf("guard: calls=%d class=%s", w.calls("probe-odd"), j2.ErrorClass)
	}
	if !strings.Contains(w.log(j1.ID), "--- Failed (permanent: video is private). Not retrying") {
		t.Fatalf("log misses the failure reason:\n%s", w.log(j1.ID))
	}
	if !strings.Contains(w.log(j2.ID), "the same error occurred twice in a row") {
		t.Fatalf("log misses the guard reason:\n%s", w.log(j2.ID))
	}
}

func pngServer(t *testing.T) *httptest.Server {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.v1712345678":
			w.Write(buf.Bytes())
		case "/html.jpg":
			w.Write([]byte("<html>nope</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Thumbnail unavailable or invalid: item done with a warning, media in the
// library (F21). A thumbnail URL with a version suffix works.
func TestScenarioThumbnails(t *testing.T) {
	srv := pngServer(t)
	bad := vid("bad", "Bad Thumb")
	bad.Thumbnail = srv.URL + "/html.jpg"
	good := vid("good", "Good Thumb")
	good.Thumbnail = srv.URL + "/ok.v1712345678"
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{"bad": bad, "good": good}}
	w := newWorld(t, sc)
	t.Setenv("FAKE_FFMPEG_FAIL", "1") // frame fallback fails too
	w.start()
	jb := w.submit(scenario.VideoPrefix+"bad", config.OptionsRequest{})
	jg := w.submit(scenario.VideoPrefix+"good", config.OptionsRequest{})
	jb = w.wait(jb.ID, store.JobCompleted, store.JobFailed)
	jg = w.wait(jg.ID, store.JobCompleted, store.JobFailed)
	if jb.State != store.JobCompleted || jg.State != store.JobCompleted {
		t.Fatalf("states %s %s\n%s", jb.State, jg.State, w.log(jb.ID))
	}
	ib := w.items(jb.ID)[0]
	if len(ib.Warnings) != 1 || ib.Warnings[0] != "No thumbnail" {
		t.Fatalf("warnings = %v", ib.Warnings)
	}
	files := strings.Join(w.libFiles(), "\n")
	for _, want := range []string{"Bad Thumb [bad]/Bad Thumb [bad].mkv", "Bad Thumb [bad]/movie.nfo", "Good Thumb [good]/folder.jpg"} {
		if !strings.Contains(files, want) {
			t.Fatalf("missing %s in library:\n%s", want, files)
		}
	}
	if strings.Contains(files, "Bad Thumb [bad]/folder.jpg") {
		t.Fatal("invalid thumbnail ended up in the library")
	}
}

// 403 on extraction: exactly one impersonation retry, then failed with the
// blocked hint.
func TestScenario403(t *testing.T) {
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{
		"blk": {ID: "blk", Title: "Blocked", Probe: []scenario.Behavior{{Exit: 1, Stderr: []string{"ERROR: [fake] blk: Unable to download webpage: HTTP Error 403: Forbidden"}}}},
		"imp": {ID: "imp", Title: "NeedsImp", Duration: 10, Probe: []scenario.Behavior{{RequireImpersonate: true}}},
	}}
	w := newWorld(t, sc)
	w.start()
	j := w.submit(scenario.VideoPrefix+"blk", config.OptionsRequest{})
	j = w.wait(j.ID, store.JobFailed)
	if w.calls("probe-blk") != 2 || j.ErrorClass != "blocked" || !strings.Contains(j.Hint, "Access denied") {
		t.Fatalf("calls=%d class=%s hint=%q", w.calls("probe-blk"), j.ErrorClass, j.Hint)
	}
	if !strings.Contains(w.log(j.ID), `Retrying with impersonation (--impersonate="")`) {
		t.Fatal("escalation not logged")
	}
	// The escalation fixes a site that needs impersonation.
	j2 := w.submit(scenario.VideoPrefix+"imp", config.OptionsRequest{})
	j2 = w.wait(j2.ID, store.JobCompleted, store.JobFailed)
	if j2.State != store.JobCompleted || !j2.Retry.Impersonated {
		t.Fatalf("state=%s retry=%+v\n%s", j2.State, j2.Retry, w.log(j2.ID))
	}
}

// Crash with traceback: at most one retry, then failed with class crash (F23).
func TestScenarioCrash(t *testing.T) {
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{
		"tb": {ID: "tb", Title: "Crash", Probe: []scenario.Behavior{{Traceback: true}}},
	}}
	w := newWorld(t, sc)
	w.start()
	j := w.submit(scenario.VideoPrefix+"tb", config.OptionsRequest{})
	j = w.wait(j.ID, store.JobFailed)
	if w.calls("probe-tb") != 2 || j.ErrorClass != "crash" || !strings.Contains(j.Error, "NoneType") {
		t.Fatalf("calls=%d class=%s err=%q", w.calls("probe-tb"), j.ErrorClass, j.Error)
	}
}

// Transient error, then success: item done after backoff.
func TestScenarioTransientThenSuccess(t *testing.T) {
	v := vid("tr", "Flaky")
	v.Download = []scenario.Behavior{{Exit: 1, Stderr: []string{"ERROR: [Errno 104] Connection reset by peer"}}, {}}
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{"tr": v}}
	w := newWorld(t, sc)
	w.start()
	j := w.submit(scenario.VideoPrefix+"tr", config.OptionsRequest{})
	j = w.wait(j.ID, store.JobCompleted, store.JobFailed)
	if j.State != store.JobCompleted || w.calls("download-tr") != 2 {
		t.Fatalf("state=%s downloads=%d\n%s", j.State, w.calls("download-tr"), w.log(j.ID))
	}
	if !strings.Contains(w.log(j.ID), "--- Attempt 1 failed (transient:") || !strings.Contains(w.log(j.ID), "Retry 1/3 at") {
		t.Fatalf("retry not logged:\n%s", w.log(j.ID))
	}
	if ok, _ := w.m.Store.HasArchive(context.Background(), "fake tr"); !ok {
		t.Fatal("archive entry missing")
	}
	arch, _ := os.ReadFile(w.env.LegacyArchivePath())
	if !strings.Contains(string(arch), "fake tr\n") {
		t.Fatalf("legacy archive not appended: %q", arch)
	}
}

// --- further behaviour ---

// A failed item never affects completed siblings (F1).
func TestPartialPlaylistKeepsCompletedItems(t *testing.T) {
	bad := vid("x2", "Two")
	bad.Download = []scenario.Behavior{{Exit: 1, Stderr: []string{"ERROR: [fake] x2: This video is DRM protected"}}}
	sc := &scenario.Scenario{
		Videos:    map[string]scenario.Video{"x1": vid("x1", "One"), "x2": bad, "x3": vid("x3", "Three")},
		Playlists: map[string]scenario.Playlist{"pl": {ID: "pl", Title: "Mixed", Entries: []string{"x1", "x2", "x3"}}},
	}
	w := newWorld(t, sc)
	w.start()
	j := w.submit(scenario.PlaylistPrefix+"pl", config.OptionsRequest{})
	j = w.wait(j.ID, store.JobPartial, store.JobFailed, store.JobCompleted)
	if j.State != store.JobPartial || j.Counts.Done != 2 || j.Counts.Failed != 1 {
		t.Fatalf("state=%s counts=%+v", j.State, j.Counts)
	}
	files := strings.Join(w.libFiles(), "\n")
	if !strings.Contains(files, "Mixed/One [x1]/One [x1].mkv") || !strings.Contains(files, "Mixed/Three [x3]/Three [x3].mkv") {
		t.Fatalf("library:\n%s", files)
	}
	if w.calls("download-x2") != 1 {
		t.Fatalf("permanent item downloaded %d times", w.calls("download-x2"))
	}
	// Retrying only touches the failed item.
	if _, err := w.m.Retry(context.Background(), j.ID, RetryRequest{}); err != nil {
		t.Fatal(err)
	}
	w.wait(j.ID, store.JobPartial)
	if w.calls("download-x1") != 1 {
		t.Fatal("completed item downloaded again on retry")
	}
}

func TestMultiAudioSubtitlesAndNFO(t *testing.T) {
	v := vid("ml", "Multi: Lang?")
	v.Languages = []string{"en", "de"}
	v.Subtitles = []string{"en", "de"}
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{"ml": v}}
	w := newWorld(t, sc)
	w.start()
	j := w.submit(scenario.VideoPrefix+"ml", config.OptionsRequest{})
	j = w.wait(j.ID, store.JobCompleted, store.JobFailed)
	if j.State != store.JobCompleted {
		t.Fatalf("%s\n%s", j.State, w.log(j.ID))
	}
	base := "Multi： Lang？ [ml]"
	files := strings.Join(w.libFiles(), "\n")
	for _, f := range []string{base + ".mkv", base + ".en.vtt", base + ".de.vtt", "movie.nfo", "folder.jpg"} {
		if !strings.Contains(files, base+"/"+f) {
			t.Fatalf("missing %s:\n%s", f, files)
		}
	}
	if !strings.Contains(w.log(j.ID), "Found 2 audio tracks: en (2ch), de (2ch)") {
		t.Fatal("audio tracks not logged")
	}
	nfo, _ := os.ReadFile(filepath.Join(w.env.LibraryRoot(), base, "movie.nfo"))
	if !strings.Contains(string(nfo), `<uniqueid type="fake.test" default="true">ml</uniqueid>`) || !strings.Contains(string(nfo), "<dateadded>") {
		t.Fatalf("nfo:\n%s", nfo)
	}
	if !strings.Contains(w.log(j.ID), "=== ytdlp-nfo-server") || !strings.Contains(w.log(j.ID), "yt-dlp 2026.01.01 (stable/2026.01.01)") {
		t.Fatal("version header missing")
	}
}

// A single-file format is remuxed into mkv; mp4 and audio-only sources
// keep the downloaded file.
func TestSingleFileFormatContainer(t *testing.T) {
	single := func(id string) scenario.Video { v := vid(id, "Single "+id); v.Single = "mp4"; return v }
	audio := vid("au", "Audio")
	audio.AudioOnly, audio.Single = true, "m4a"
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{"s1": single("s1"), "s2": single("s2"), "au": audio}}
	w := newWorld(t, sc)
	w.start()
	mp4 := "mp4"
	for _, c := range []struct {
		id  string
		req config.OptionsRequest
	}{{"s1", config.OptionsRequest{}}, {"s2", config.OptionsRequest{Container: &mp4}}, {"au", config.OptionsRequest{}}} {
		j := w.submit(scenario.VideoPrefix+c.id, c.req)
		if j = w.wait(j.ID, store.JobCompleted, store.JobFailed); j.State != store.JobCompleted {
			t.Fatalf("%s: %s\n%s", c.id, j.State, w.log(j.ID))
		}
	}
	files := strings.Join(w.libFiles(), "\n")
	for _, f := range []string{"Single s1 [s1]/Single s1 [s1].mkv", "Single s2 [s2]/Single s2 [s2].mp4", "Audio [au]/Audio [au].m4a"} {
		if !strings.Contains(files, f) {
			t.Fatalf("missing %s:\n%s", f, files)
		}
	}
}

func TestVerifyFailureRetried(t *testing.T) {
	v := vid("vf", "Verify")
	v.Download = []scenario.Behavior{{BadMedia: true}, {}}
	w := newWorld(t, &scenario.Scenario{Videos: map[string]scenario.Video{"vf": v}})
	w.start()
	j := w.submit(scenario.VideoPrefix+"vf", config.OptionsRequest{})
	j = w.wait(j.ID, store.JobCompleted, store.JobFailed)
	if j.State != store.JobCompleted || !strings.Contains(w.log(j.ID), "verification failed") {
		t.Fatalf("%s\n%s", j.State, w.log(j.ID))
	}
}

func TestExtractorBrokenRetriesWithNightly(t *testing.T) {
	v := vid("eb", "Broken")
	v.Download = []scenario.Behavior{{RequireVersion: "2026.10.10.1"}}
	w := newWorld(t, &scenario.Scenario{Videos: map[string]scenario.Video{"eb": v}})
	w.start()
	j := w.submit(scenario.VideoPrefix+"eb", config.OptionsRequest{})
	j = w.wait(j.ID, store.JobCompleted, store.JobFailed)
	if j.State != store.JobCompleted || j.YtdlpVersion != "nightly/2026.10.10.1" {
		t.Fatalf("state=%s version=%s\n%s", j.State, j.YtdlpVersion, w.log(j.ID))
	}
	if !strings.Contains(w.log(j.ID), "Retrying with yt-dlp 2026.10.10.1 (nightly/2026.10.10.1)") {
		t.Fatal("nightly retry not logged")
	}
}

func TestRateLimitedCooldown(t *testing.T) {
	v := vid("rl", "Limited")
	v.Probe = []scenario.Behavior{{Exit: 1, Stderr: []string{"ERROR: [fake] rl: HTTP Error 429: Too Many Requests"}}, {}}
	w := newWorld(t, &scenario.Scenario{Videos: map[string]scenario.Video{"rl": v}})
	w.start()
	j := w.submit(scenario.VideoPrefix+"rl", config.OptionsRequest{})
	j = w.wait(j.ID, store.JobCompleted, store.JobFailed)
	if j.State != store.JobCompleted || !strings.Contains(w.log(j.ID), "rate_limited") {
		t.Fatalf("%s\n%s", j.State, w.log(j.ID))
	}
}

func TestDuplicateSubmit(t *testing.T) {
	v := vid("dup", "Dup")
	v.Download = []scenario.Behavior{{Hang: true}}
	w := newWorld(t, &scenario.Scenario{Videos: map[string]scenario.Video{"dup": v}})
	w.start()
	w.submit(scenario.VideoPrefix+"dup", config.OptionsRequest{})
	_, err := w.m.Submit(context.Background(), "http://fake.test/v/dup#x", config.OptionsRequest{})
	var de *DuplicateError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v", err)
	}
	for _, bad := range []string{"--exec=rm", "file:///etc/passwd", "-o x"} {
		if _, err := w.m.Submit(context.Background(), bad, config.OptionsRequest{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
}

// Cancel and delete during a download stop the process group before files
// are removed (F9).
func TestCancelAndDeleteDuringDownload(t *testing.T) {
	a := vid("ca", "Cancel me")
	a.Download = []scenario.Behavior{{Hang: true, WritePart: true}}
	b := vid("de", "Delete me")
	b.Download = []scenario.Behavior{{Hang: true, WritePart: true}}
	w := newWorld(t, &scenario.Scenario{Videos: map[string]scenario.Video{"ca": a, "de": b}})
	m := w.start()
	ja := w.submit(scenario.VideoPrefix+"ca", config.OptionsRequest{})
	jb := w.submit(scenario.VideoPrefix+"de", config.OptionsRequest{})
	waitItemState(t, m, ja.ID, store.ItemDownloading)
	waitItemState(t, m, jb.ID, store.ItemDownloading)
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if _, err := m.Cancel(context.Background(), ja.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), jb.ID); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("cancel took too long")
	}
	j, _ := m.Store.GetJob(context.Background(), ja.ID)
	if j.State != store.JobCancelled {
		t.Fatalf("state = %s", j.State)
	}
	if _, err := m.Store.GetJob(context.Background(), jb.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("deleted job still exists")
	}
	if _, err := os.Stat(filepath.Join(w.env.WorkRoot(), fmt.Sprint(jb.ID))); err == nil {
		t.Fatal("work dir of deleted job left behind")
	}
	if st := m.Sched.Status(); len(st.Running) != 0 {
		t.Fatalf("still running: %v", st.Running)
	}
}

func waitItemState(t *testing.T, m *Manager, jobID int64, st store.ItemState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		its, _ := m.Store.ItemsForJob(context.Background(), jobID)
		for _, it := range its {
			if it.State == st {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %d: no item reached %s", jobID, st)
}

// The same video submitted twice in parallel is downloaded once.
func TestSameVideoInParallel(t *testing.T) {
	v := vid("same", "Same")
	v.Download = []scenario.Behavior{{SleepMs: 300}}
	sc := &scenario.Scenario{
		Videos:    map[string]scenario.Video{"same": v},
		Playlists: map[string]scenario.Playlist{"wrap": {ID: "wrap", Title: "Wrap", Entries: []string{"same"}}},
	}
	w := newWorld(t, sc)
	w.start()
	j1 := w.submit(scenario.VideoPrefix+"same", config.OptionsRequest{})
	j2 := w.submit(scenario.PlaylistPrefix+"wrap", config.OptionsRequest{})
	j1 = w.wait(j1.ID, store.JobCompleted, store.JobFailed)
	j2 = w.wait(j2.ID, store.JobCompleted, store.JobFailed)
	if j1.State != store.JobCompleted || j2.State != store.JobCompleted {
		t.Fatalf("%s %s", j1.State, j2.State)
	}
	if w.calls("download-same") != 1 {
		t.Fatalf("downloaded %d times", w.calls("download-same"))
	}
}

// Shutdown during a download: the job resumes after a restart.
func TestShutdownAndResume(t *testing.T) {
	v := vid("rs", "Resume")
	v.Download = []scenario.Behavior{{Hang: true, WritePart: true}, {}}
	w := newWorld(t, &scenario.Scenario{Videos: map[string]scenario.Video{"rs": v}})
	m := w.start()
	j := w.submit(scenario.VideoPrefix+"rs", config.OptionsRequest{})
	waitItemState(t, m, j.ID, store.ItemDownloading)
	time.Sleep(100 * time.Millisecond)
	w.stop()
	m = w.start()
	j = w.wait(j.ID, store.JobCompleted, store.JobFailed)
	if j.State != store.JobCompleted {
		t.Fatalf("%s\n%s", j.State, w.log(j.ID))
	}
	if !strings.Contains(w.log(j.ID), "Interrupted by server shutdown") {
		t.Fatal("shutdown not logged")
	}
}

// v1 upgrade: jobs.json and the archive are imported, the legacy layout is
// kept and a different video with the same title gets " (2)".
func TestV1Migration(t *testing.T) {
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{"q1": vid("q1", "Same Title"), "old": vid("old", "Old")}}
	w := newWorld(t, sc)
	os.MkdirAll(w.env.DataDir, 0o755)
	os.MkdirAll(w.env.DownloadDir, 0o755)
	os.WriteFile(filepath.Join(w.env.DataDir, "jobs.json"), []byte(`{"nextId":20,"jobs":[
		{"id":"19","url":"`+scenario.VideoPrefix+`q1","status":"retrying","createdAt":"2026-01-01T00:00:00Z","options":{"format":"mkv","allAudio":true,"subtitles":true}},
		{"id":"18","url":"`+scenario.VideoPrefix+`old","status":"completed","createdAt":"2026-01-01T00:00:00Z","doneAt":"2026-01-01T00:01:00Z","options":{"format":"mkv","allAudio":true,"subtitles":true}}]}`), 0o644)
	os.WriteFile(w.env.LegacyArchivePath(), []byte("fake old\n"), 0o644)
	// An existing library folder with the same title but another video.
	existing := filepath.Join(w.env.LibraryRoot(), "Same Title")
	os.MkdirAll(existing, 0o755)
	os.WriteFile(filepath.Join(existing, "movie.nfo"), []byte(`<movie><uniqueid type="x" default="true">someone-else</uniqueid></movie>`), 0o644)
	os.WriteFile(filepath.Join(existing, "Same Title.mkv"), []byte("OTHER"), 0o644)

	m := w.start()
	s := m.Settings.Settings()
	if s.Naming.Preset != config.NamingLegacy || s.Ytdlp.Channel != config.ChannelStable {
		t.Fatalf("settings = %+v", s.Naming)
	}
	j := w.wait(19, store.JobCompleted, store.JobFailed)
	if j.State != store.JobCompleted || !j.Legacy {
		t.Fatalf("job 19 = %s\n%s", j.State, w.log(19))
	}
	old, err := m.Store.GetJob(context.Background(), 18)
	if err != nil || old.State != store.JobCompleted {
		t.Fatal("history not imported")
	}
	if ok, _ := m.Store.HasArchive(context.Background(), "fake old"); !ok {
		t.Fatal("archive not imported")
	}
	files := strings.Join(w.libFiles(), "\n")
	if !strings.Contains(files, "Same Title (2)/Same Title (2).mkv") {
		t.Fatalf("library:\n%s", files)
	}
	if b, _ := os.ReadFile(filepath.Join(existing, "Same Title.mkv")); string(b) != "OTHER" {
		t.Fatal("existing library folder modified (F3)")
	}
	if _, err := os.Stat(filepath.Join(w.env.DataDir, "jobs.json.v1-backup")); err != nil {
		t.Fatal("no backup of jobs.json")
	}
	nj := w.submit(scenario.VideoPrefix+"q1?again=1", config.OptionsRequest{})
	if nj.ID != 21 && nj.ID != 20 {
		t.Fatalf("new job id %d, want > 19", nj.ID)
	}
}

// Force re-download replaces the library folder of the same video in place.
func TestForceRedownloadReplaces(t *testing.T) {
	w := newWorld(t, &scenario.Scenario{Videos: map[string]scenario.Video{"fr": vid("fr", "Force")}})
	m := w.start()
	j := w.submit(scenario.VideoPrefix+"fr", config.OptionsRequest{})
	w.wait(j.ID, store.JobCompleted)
	f := false
	if _, err := m.Retry(context.Background(), j.ID, RetryRequest{FailedOnly: &f}); err != nil {
		t.Fatal(err)
	}
	j = w.wait(j.ID, store.JobCompleted, store.JobFailed)
	if j.State != store.JobCompleted || w.calls("download-fr") != 2 {
		t.Fatalf("%s downloads=%d\n%s", j.State, w.calls("download-fr"), w.log(j.ID))
	}
	files := w.libFiles()
	if len(files) != 3 || !strings.Contains(strings.Join(files, "\n"), "Force [fr]/Force [fr].mkv") {
		t.Fatalf("library: %v", files)
	}
}

// Delete-all while downloads run: every process group is gone before the
// records and work dirs are removed, and the slot count stays correct (F8).
func TestDeleteAllDuringDownloads(t *testing.T) {
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{}}
	for i := 0; i < 6; i++ {
		v := vid(fmt.Sprintf("h%d", i), fmt.Sprintf("Hang %d", i))
		v.Download = []scenario.Behavior{{Hang: true, WritePart: true}}
		sc.Videos[v.ID] = v
	}
	w := newWorld(t, sc)
	m := w.start()
	for i := 0; i < 6; i++ {
		w.submit(fmt.Sprintf("%sh%d", scenario.VideoPrefix, i), config.OptionsRequest{})
	}
	waitItemState(t, m, 1, store.ItemDownloading)
	if err := m.DeleteAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ids, _ := m.Store.JobIDs(context.Background())
	if len(ids) != 0 {
		t.Fatalf("jobs left: %v", ids)
	}
	if entries, _ := os.ReadDir(w.env.WorkRoot()); len(entries) != 0 {
		t.Fatalf("work dirs left: %d", len(entries))
	}
	if st := m.Sched.Status(); len(st.Running) != 0 || st.Queued != 0 {
		t.Fatalf("scheduler not empty: %+v", st)
	}
	// The limit still works afterwards.
	w.submit(scenario.VideoPrefix+"h0", config.OptionsRequest{})
	waitItemState(t, m, 7, store.ItemDownloading)
	if st := m.Sched.Status(); len(st.Running) != 1 {
		t.Fatalf("running after delete-all: %v", st.Running)
	}
}

// Bulk import of 500 URLs stays fast (§14 phase 4 exit criterion): the
// duplicate check and the inserts use a constant number of queries.
func TestBulk500(t *testing.T) {
	w := newWorld(t, &scenario.Scenario{Videos: map[string]scenario.Video{}})
	m := w.start()
	m.Sched.Pause("test", time.Time{}) // measure submitting, not downloading
	urls := make([]string, 500)
	for i := range urls {
		urls[i] = fmt.Sprintf("%sbulk%d", scenario.VideoPrefix, i)
	}
	start := time.Now()
	res := m.SubmitBulk(context.Background(), urls, config.OptionsRequest{})
	took := time.Since(start)
	t.Logf("submitting 500 URLs took %v", took)
	created := 0
	for _, r := range res {
		if r.Job != nil && r.Job.ID != 0 {
			created++
		}
	}
	if created != 500 {
		t.Fatalf("created %d", created)
	}
	// Generous bound for slow CI runners with -race; locally this takes a
	// fraction of it.
	if took > 30*time.Second {
		t.Fatalf("bulk submit took %v", took)
	}
	// A second import of the same URLs is detected as duplicates.
	res = m.SubmitBulk(context.Background(), urls[:10], config.OptionsRequest{})
	for _, r := range res {
		if r.Dup == nil || r.Dup.ID == 0 {
			t.Fatalf("not reported as duplicate of an existing job: %+v", r)
		}
	}
	start = time.Now()
	jobs, err := m.List(context.Background(), ListFilter{Tab: "queued", Limit: 50})
	if err != nil || len(jobs) != 50 {
		t.Fatalf("list: %d %v", len(jobs), err)
	}
	t.Logf("listing the first page took %v", time.Since(start))
	if c, _ := m.Counts(context.Background()); c["queued"] != 500 {
		t.Fatalf("counts %v", c)
	}
}

// Library check: archive entries without library folder (e.g. v1 deleted a
// partly failed playlist, F1) are found and can be removed / re-queued.
func TestArchiveCheckAndFix(t *testing.T) {
	sc := &scenario.Scenario{Videos: map[string]scenario.Video{"ddd": vid("ddd", "Lost")}}
	w := newWorld(t, sc)
	lib := w.env.LibraryRoot()
	mk := func(rel string, files map[string]string) {
		dir := filepath.Join(lib, rel)
		os.MkdirAll(dir, 0o755)
		for n, c := range files {
			os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644)
		}
	}
	nfo := func(id string) string {
		return `<movie><uniqueid type="x" default="true">` + id + `</uniqueid></movie>`
	}
	mk("Playlist/Ok", map[string]string{"movie.nfo": nfo("aaa"), "Ok.mkv": "media"})
	mk("Broken", map[string]string{"movie.nfo": nfo("ccc")})
	// A v1 job dir with a finished item that never reached OUTPUT_DIR.
	stranded := filepath.Join(w.env.DownloadDir, "15", "Stranded")
	os.MkdirAll(stranded, 0o755)
	os.WriteFile(filepath.Join(stranded, "movie.nfo"), []byte(nfo("eee")), 0o644)
	os.WriteFile(filepath.Join(stranded, "Stranded.mkv"), []byte("media"), 0o644)
	os.MkdirAll(w.env.DownloadDir, 0o755)
	os.WriteFile(w.env.LegacyArchivePath(), []byte("youtube aaa\nyoutube bbb\nfake ccc\nfake ddd\nfake eee\n"), 0o644)

	m := w.start()
	ctx := context.Background()
	// "fake ddd" was downloaded by the server once (URL known from its item)
	// and its folder was deleted afterwards.
	j := &store.Job{URL: scenario.VideoPrefix + "ddd", State: store.JobCompleted}
	m.Store.CreateJob(ctx, j)
	m.Store.CreateItems(ctx, []*store.Item{{JobID: j.ID, URL: scenario.VideoPrefix + "ddd", ArchiveKey: "fake ddd", State: store.ItemDone}})

	r, err := m.CheckArchive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Entries != 5 || r.OK != 1 || r.Missing != 3 || r.Incomplete != 1 {
		t.Fatalf("check = %+v", r)
	}
	probs := map[string]ArchiveProblem{}
	for _, p := range r.Problems {
		probs[p.Key] = p
	}
	if p := probs["youtube bbb"]; p.Status != CheckMissing || p.URL != "https://www.youtube.com/watch?v=bbb" {
		t.Fatalf("bbb = %+v", p)
	}
	if p := probs["fake ccc"]; p.Status != CheckIncomplete || p.Path != "Broken" {
		t.Fatalf("ccc = %+v", p)
	}
	if p := probs["fake ddd"]; p.URL != scenario.VideoPrefix+"ddd" {
		t.Fatalf("ddd = %+v", p)
	}
	if p := probs["fake eee"]; p.Stranded != stranded || p.URL != "" {
		t.Fatalf("eee = %+v", p)
	}

	// Remove bbb and eee only; re-download ddd.
	res, err := m.FixArchive(ctx, []string{"youtube bbb", "fake eee"}, false)
	if err != nil || res.Removed != 2 || res.Queued != 0 {
		t.Fatalf("fix = %+v %v", res, err)
	}
	res, err = m.FixArchive(ctx, []string{"fake ddd"}, true)
	if err != nil || res.Removed != 1 || res.Queued != 1 {
		t.Fatalf("fix redownload = %+v %v", res, err)
	}
	arch, _ := os.ReadFile(w.env.LegacyArchivePath())
	for _, gone := range []string{"youtube bbb", "fake eee"} {
		if strings.Contains(string(arch), gone) {
			t.Fatalf("%s still in legacy archive: %q", gone, arch)
		}
	}
	// The re-queued download finishes and is archived again.
	ids, _ := m.Store.JobIDs(ctx, store.JobQueued, store.JobProbing, store.JobRunning, store.JobCompleted)
	var newJob int64
	for _, id := range ids {
		if id != j.ID {
			newJob = id
		}
	}
	w.wait(newJob, store.JobCompleted)
	if ok, _ := m.Store.HasArchive(ctx, "fake ddd"); !ok {
		t.Fatal("re-downloaded video not archived")
	}
	// Library files and the stranded dir were not touched.
	if _, err := os.Stat(filepath.Join(lib, "Broken", "movie.nfo")); err != nil {
		t.Fatal("library file touched")
	}
	if _, err := os.Stat(filepath.Join(stranded, "Stranded.mkv")); err != nil {
		t.Fatal("stranded file touched")
	}
	r, _ = m.CheckArchive(ctx)
	if r.Missing != 0 || r.Incomplete != 1 || r.OK != 2 {
		t.Fatalf("after fix: %+v", r)
	}
}
