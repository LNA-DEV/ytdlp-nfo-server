package subscriptions

import (
	"testing"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

func TestYouTubeChannel(t *testing.T) {
	cases := []struct {
		in, base, tab string
		ok            bool
	}{
		{"https://www.youtube.com/@name", "https://www.youtube.com/@name", "", true},
		{"https://youtube.com/@name/videos?si=x", "https://www.youtube.com/@name", "videos", true},
		{"https://m.youtube.com/channel/UC0123456789abcdefghij_-/shorts/", "https://www.youtube.com/channel/UC0123456789abcdefghij_-", "shorts", true},
		{"https://www.youtube.com/c/Name/streams", "https://www.youtube.com/c/Name", "streams", true},
		{"https://www.youtube.com/user/Name/featured", "https://www.youtube.com/user/Name", "", true},
		{"https://www.youtube.com/@name/playlists", "", "", false},
		{"https://www.youtube.com/watch?v=abc", "", "", false},
		{"https://www.youtube.com/playlist?list=PL1", "", "", false},
		{"https://example.com/@name/videos", "", "", false},
	}
	for _, c := range cases {
		base, tab, ok := YouTubeChannel(c.in)
		if base != c.base || tab != c.tab || ok != c.ok {
			t.Errorf("%s: got %q %q %v", c.in, base, tab, ok)
		}
	}
}

func TestCanonical(t *testing.T) {
	const id = "UC0123456789abcdefghij_-"
	l := &ytdlp.Listing{ID: id, ChannelID: id}
	for _, u := range []string{"https://www.youtube.com/@name/videos", "https://youtube.com/channel/" + id + "/videos"} {
		if c, tab := Canonical(u, l); c != ChannelTabURL(id, "videos") || tab != "videos" {
			t.Errorf("%s: %s %s", u, c, tab)
		}
	}
	// Without a listing: from the URL if it names the channel id.
	if c, _ := Canonical("https://www.youtube.com/channel/"+id+"/streams", nil); c != ChannelTabURL(id, "streams") {
		t.Errorf("channel URL without listing: %s", c)
	}
	if c, tab := Canonical("https://www.youtube.com/@name/shorts", nil); c != "https://youtube.com/@name/shorts" || tab != "shorts" {
		t.Errorf("handle without listing: %s %s", c, tab)
	}
	if c, tab := Canonical("https://www.youtube.com/playlist?list=PL1&si=x", &ytdlp.Listing{ID: "PL1"}); c != "https://youtube.com/playlist?list=PL1" || tab != "" {
		t.Errorf("playlist: %s %s", c, tab)
	}
	if Kind(l) != "channel" || Kind(&ytdlp.Listing{ID: "PL1", ChannelID: id}) != "playlist" {
		t.Error("kind")
	}
	if DisplayName(&ytdlp.Listing{ID: id, ChannelID: id, Channel: "Chan", Title: "Chan - Videos"}) != "Chan" {
		t.Error("display name of a channel")
	}
}
