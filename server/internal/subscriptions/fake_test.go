package subscriptions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/scenario"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
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
	testutil.Cleanup()
	os.Exit(code)
}

// useScenario points the fake yt-dlp at sc for this test.
func useScenario(t *testing.T, sc *scenario.Scenario) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.json")
	if err := sc.Write(path); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	os.MkdirAll(state, 0o755)
	t.Setenv(scenario.EnvScenario, path)
	t.Setenv(scenario.EnvState, state)
}

func list(t *testing.T, url string, spec ytdlp.ListSpec) *ytdlp.Listing {
	t.Helper()
	res := ytdlp.Run(context.Background(), ytdlp.Call{Bin: filepath.Join(fakeDir, "yt-dlp"),
		Args: ytdlp.ListArgs(url, spec, ytdlp.CallOptions{}), CaptureStdout: true})
	if !res.OK() {
		t.Fatalf("listing failed: %+v", res)
	}
	l, err := ytdlp.ParseListing(res.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func keys(l *ytdlp.Listing) []string {
	var out []string
	for _, e := range l.Entries {
		out = append(out, e.ID+":"+e.LiveStatus)
	}
	return out
}

// The fake yt-dlp returns a different listing per call and honours -I.
func TestFakeListings(t *testing.T) {
	const id = "UC0123456789abcdefghij_-"
	useScenario(t, &scenario.Scenario{
		Videos: map[string]scenario.Video{"a": {ID: "a", Title: "A", Duration: 60, Timestamp: 1700000000, UploadDate: "20231114"}},
		Playlists: map[string]scenario.Playlist{"chan": {ID: id, Title: "Chan - Videos", ChannelID: id, ExtractorKey: "YoutubeTab",
			Listings: [][]string{{"a", "b", "c"}, {"d|live_status=is_upcoming", "a", "b", "c"}}}},
		Aliases: map[string]string{"https://www.youtube.com/@chan/videos": scenario.PlaylistPrefix + "chan"},
	})
	l := list(t, "https://www.youtube.com/@chan/videos", ytdlp.ListSpec{Window: 2})
	if fmt.Sprint(keys(l)) != "[a: b:]" || l.ChannelID != id || l.Entries[0].Duration != 60 || l.Entries[0].UploadDate != "20231114" {
		t.Fatalf("first listing: %v %+v", keys(l), l.Entries[0])
	}
	if c, tab := Canonical("https://www.youtube.com/@chan/videos", l); c != ChannelTabURL(id, TabVideos) || tab != TabVideos {
		t.Fatalf("canonical %s %s", c, tab)
	}
	l = list(t, scenario.PlaylistPrefix+"chan", ytdlp.ListSpec{})
	if fmt.Sprint(keys(l)) != "[d:is_upcoming a: b: c:]" {
		t.Fatalf("second listing: %v", keys(l))
	}
	l = list(t, scenario.PlaylistPrefix+"chan", ytdlp.ListSpec{Window: 10})
	if len(l.Entries) != 4 {
		t.Fatalf("the last listing repeats: %v", keys(l))
	}
}
