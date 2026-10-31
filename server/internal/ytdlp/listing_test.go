package ytdlp

import (
	"slices"
	"strings"
	"testing"
)

func TestListArgs(t *testing.T) {
	o := CallOptions{CacheDir: "/c", Impersonate: "any"}
	got := ListArgs("https://www.youtube.com/channel/UC1/videos", ListSpec{Window: 50, YouTubeTab: true}, o)
	s := strings.Join(got, " ")
	for _, want := range []string{"--ignore-config", "--impersonate=", "--flat-playlist -J --lazy-playlist -I :50",
		"--extractor-args youtubetab:approximate_date -- https://www.youtube.com/channel/UC1/videos"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	full := ListArgs("https://example.com/pl", ListSpec{}, CallOptions{})
	if slices.Contains(full, "--lazy-playlist") || slices.Contains(full, "-I") || slices.Contains(full, "--extractor-args") {
		t.Fatalf("full listing: %v", full)
	}
	if full[len(full)-2] != "--" {
		t.Fatalf("URL must follow --: %v", full)
	}
}

func TestParseListing(t *testing.T) {
	data := `{"_type": "playlist", "id": "UCabc", "title": "Chan - Videos", "channel": "Chan", "channel_id": "UCabc",
	"uploader_id": "@chan", "channel_url": "https://www.youtube.com/channel/UCabc", "extractor_key": "YoutubeTab",
	"playlist_count": 412,
	"thumbnails": [{"url": "https://i/banner.jpg", "width": 1000, "id": "banner_uncropped"}, {"url": "https://i/avatar.jpg", "id": "avatar_uncropped"}],
	"entries": [
	 {"_type": "url", "ie_key": "Youtube", "id": "v1", "url": "https://www.youtube.com/watch?v=v1", "title": "One",
	  "duration": 61.5, "timestamp": 1700000000, "upload_date": "20231114", "live_status": "is_upcoming",
	  "availability": "subscriber_only", "thumbnails": [{"url": "https://i/s.jpg", "width": 120}, {"url": "https://i/l.jpg", "width": 480}]},
	 {"_type": "url", "ie_key": "YoutubeTab", "id": "PL1", "url": "https://www.youtube.com/playlist?list=PL1", "title": "A list"},
	 {"_type": "url", "id": 42, "url": "https://example.com/v/42", "upload_date": "bad"}
	]}`
	l, err := ParseListing([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if !l.IsPlaylist || l.ChannelID != "UCabc" || l.UploaderID != "@chan" || l.Count != 412 || l.Thumbnail != "https://i/avatar.jpg" || l.Channel != "Chan" {
		t.Fatalf("listing: %+v", l)
	}
	if len(l.Entries) != 3 {
		t.Fatalf("entries: %d", len(l.Entries))
	}
	e := l.Entries[0]
	if e.Key() != "youtube v1" || e.Duration != 61.5 || e.Timestamp != 1700000000 || e.UploadDate != "20231114" ||
		e.LiveStatus != "is_upcoming" || e.Availability != "subscriber_only" || e.Thumbnail != "https://i/l.jpg" || e.Nested {
		t.Fatalf("entry 0: %+v", e)
	}
	if !l.Entries[1].Nested {
		t.Fatal("tab entry not nested")
	}
	if e := l.Entries[2]; e.ID != "42" || e.Key() != "" || e.UploadDate != "" {
		t.Fatalf("entry 2: %+v", e)
	}

	single, err := ParseListing([]byte(`{"_type": "video", "id": "v9", "extractor_key": "Youtube", "title": "Nine",
	"webpage_url": "https://www.youtube.com/watch?v=v9", "channel_url": "https://www.youtube.com/channel/UCx", "duration": 5}`))
	if err != nil {
		t.Fatal(err)
	}
	if single.IsPlaylist || single.ChannelURL != "https://www.youtube.com/channel/UCx" || len(single.Entries) != 1 ||
		single.Entries[0].Key() != "youtube v9" || single.Entries[0].URL != "https://www.youtube.com/watch?v=v9" {
		t.Fatalf("single: %+v", single)
	}
}

// Some extractors return complete videos as playlist entries: their "url"
// is the media file of the chosen format, their page is "webpage_url".
// Listings and probes take the page, which keeps the site's extractor and
// key (a download of the file would be "generic <id>").
func TestCompleteEntriesUsePageURL(t *testing.T) {
	raw := []byte(`{"_type": "playlist", "id": "someone", "title": "someone", "entries": [
		{"_type": "url", "ie_key": "Example", "id": "flat1", "url": "https://example.com/watch/flat1"},
		{"id": "full2", "extractor_key": "Example", "title": "Full", "webpage_url": "https://example.com/watch/full2",
		 "url": "https://media.example.com/Full2.mp4", "formats": [{"url": "https://media.example.com/Full2.mp4"}]},
		{"id": "full3", "extractor_key": "Example", "url": "https://example.com/watch/full3"}
	]}`)
	l, err := ParseListing(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://example.com/watch/flat1", "https://example.com/watch/full2", "https://example.com/watch/full3"}
	for i, e := range l.Entries {
		if e.URL != want[i] {
			t.Errorf("listing entry %d: %s, want %s", i, e.URL, want[i])
		}
	}
	pr, err := ParseProbe(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range pr.Entries {
		if e.URL != want[i] {
			t.Errorf("probe entry %d: %s, want %s", i, e.URL, want[i])
		}
	}
	if pr.Entries[1].Key() != "example full2" {
		t.Errorf("key %q", pr.Entries[1].Key())
	}
}

// An extractor that lists complete metadata: yt-dlp puts the media URL of
// the best format into "url", the page is webpage_url. An entry without its
// own page gets the playlist's page filled in, so it keeps its url.
func TestResolvedEntries(t *testing.T) {
	data := `{"_type": "playlist", "id": "someone", "title": "someone", "extractor_key": "FakeUser",
	"webpage_url": "https://example.com/users/someone",
	"entries": [
	 {"id": "c1", "extractor_key": "Fake", "title": "One", "timestamp": 1760000000, "duration": 16, "format_id": "hd",
	  "url": "https://media.example.com/C1.mp4", "webpage_url": "https://example.com/watch/c1",
	  "playlist_webpage_url": "https://example.com/users/someone"},
	 {"id": "c2", "extractor_key": "FakeUser", "url": "https://media.example.com/C2.mp4",
	  "webpage_url": "https://example.com/users/someone", "playlist_webpage_url": "https://example.com/users/someone"}
	]}`
	l, err := ParseListing([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if e := l.Entries[0]; e.URL != "https://example.com/watch/c1" || e.Key() != "fake c1" || e.Nested {
		t.Fatalf("resolved entry: %+v", e)
	}
	if e := l.Entries[1]; e.URL != "https://media.example.com/C2.mp4" {
		t.Fatalf("resolved entry without its own page: %+v", e)
	}
	pr, err := ParseProbe([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Entries) != 2 || pr.Entries[0].URL != "https://example.com/watch/c1" || pr.Entries[1].URL != "https://media.example.com/C2.mp4" {
		t.Fatalf("probe entries: %+v", pr.Entries)
	}
}
