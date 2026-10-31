package formats

import (
	"encoding/json"
	"testing"
)

func parse(t *testing.T, s string) []map[string]any {
	t.Helper()
	var f []map[string]any
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

const multiLang = `[
 {"format_id":"137","vcodec":"avc1","acodec":"none"},
 {"format_id":"251-0","language":"en","vcodec":"none","acodec":"opus","abr":130,"audio_channels":2},
 {"format_id":"140-0","language":"en","vcodec":"none","acodec":"mp4a","abr":128,"audio_channels":2},
 {"format_id":"251-1","language":"de","vcodec":"none","acodec":"opus","abr":120},
 {"format_id":"258","language":"en","vcodec":"none","acodec":"mp4a","abr":384,"audio_channels":6},
 {"format_id":"18","language":"en","vcodec":"avc1","acodec":"mp4a"},
 {"format_id":"nolang","vcodec":"none","acodec":"opus","abr":999},
 {"format_id":"nullv","language":"fr","vcodec":null,"acodec":"opus","abr":200},
 {"format_id":"nulla","language":"it","acodec":null,"abr":50}
]`

func TestSelectAudioTracks(t *testing.T) {
	tracks := SelectAudioTracks(parse(t, multiLang))
	// Same rule as ytdlp-nfo: best abr per (language, channels); missing
	// vcodec counts as "none", explicit null vcodec does not; null acodec
	// counts as audio.
	want := []AudioTrack{
		{Language: "en", Channels: 2, FormatID: "251-0", ABR: 130},
		{Language: "de", Channels: 2, FormatID: "251-1", ABR: 120},
		{Language: "en", Channels: 6, FormatID: "258", ABR: 384},
		{Language: "it", Channels: 2, FormatID: "nulla", ABR: 50},
	}
	if len(tracks) != len(want) {
		t.Fatalf("got %+v", tracks)
	}
	for i := range want {
		if tracks[i] != want[i] {
			t.Errorf("track %d = %+v, want %+v", i, tracks[i], want[i])
		}
	}
}

func TestSelect(t *testing.T) {
	f := parse(t, multiLang)
	s := Select(f, true, "res:1080")
	if s.Format != "bv+251-0+251-1+258+nulla/bv*+ba/best" || !s.AudioMultistream || s.Sort != "res:1080" {
		t.Fatalf("got %+v", s)
	}
	if s := Select(f, false, ""); s.Format != "bv*+ba/best" || s.AudioMultistream {
		t.Fatalf("got %+v", s)
	}
	single := parse(t, `[{"format_id":"a","language":"en","vcodec":"none","acodec":"opus"}]`)
	if s := Select(single, true, ""); s.Format != "bv*+ba/best" || s.AudioMultistream {
		t.Fatalf("got %+v", s)
	}
}

func TestDescribe(t *testing.T) {
	f := parse(t, multiLang)
	c := Describe(f, "137+251-0+251-1")
	if c.Video != 1 || c.Audio != 2 || !c.Known {
		t.Fatalf("got %+v", c)
	}
	c = Describe(f, "18")
	if c.Video != 1 || c.Audio != 1 {
		t.Fatalf("got %+v", c)
	}
	if AudioOnlySource(f) {
		t.Fatal("has video")
	}
	if !AudioOnlySource(parse(t, `[{"format_id":"a","vcodec":"none"}]`)) ||
		!AudioOnlySource(parse(t, `[{"format_id":"a","ext":"m4a","video_ext":"none","audio_ext":"m4a"}]`)) {
		t.Fatal("audio only")
	}
	// Formats without codec fields (sites that only list a file per
	// quality): video to yt-dlp, so not audio only.
	for _, fs := range []string{`[{"format_id":"hd","ext":"mp4","video_ext":"mp4","audio_ext":"none"}]`, `[{"format_id":"hd","ext":"mp4"}]`} {
		if AudioOnlySource(parse(t, fs)) {
			t.Fatalf("no codec fields counted as audio only: %s", fs)
		}
	}
}
