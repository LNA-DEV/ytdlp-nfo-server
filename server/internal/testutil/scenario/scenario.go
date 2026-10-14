// Package scenario describes how the fake yt-dlp used in tests behaves
// (§15). Tests write a Scenario as JSON and point FAKE_YTDLP_SCENARIO at it.
package scenario

import (
	"encoding/json"
	"os"
)

// Env variables read by the fake binary.
const (
	EnvScenario = "FAKE_YTDLP_SCENARIO"
	EnvState    = "FAKE_YTDLP_STATE"
)

// Behavior is what one attempt of a call does. The n-th attempt uses the
// n-th behavior; the last one repeats.
type Behavior struct {
	Exit               int      `json:"exit,omitempty"`
	Stderr             []string `json:"stderr,omitempty"`
	Hang               bool     `json:"hang,omitempty"`
	RequireImpersonate bool     `json:"requireImpersonate,omitempty"`
	RequireVersion     string   `json:"requireVersion,omitempty"`
	WritePart          bool     `json:"writePart,omitempty"`
	NoMedia            bool     `json:"noMedia,omitempty"`
	BadMedia           bool     `json:"badMedia,omitempty"`
	Traceback          bool     `json:"traceback,omitempty"`
	SleepMs            int      `json:"sleepMs,omitempty"`
}

// Video is a fake video.
type Video struct {
	ID         string           `json:"id"`
	Title      string           `json:"title"`
	Uploader   string           `json:"uploader,omitempty"`
	Duration   float64          `json:"duration"`
	Thumbnail  string           `json:"thumbnail,omitempty"`
	Thumbnails []map[string]any `json:"thumbnails,omitempty"`
	Subtitles  []string         `json:"subtitles,omitempty"`
	Languages  []string         `json:"languages,omitempty"` // audio tracks
	AudioOnly  bool             `json:"audioOnly,omitempty"`
	SizeBytes  int64            `json:"sizeBytes,omitempty"`
	Probe      []Behavior       `json:"probe,omitempty"`
	Meta       []Behavior       `json:"meta,omitempty"`
	Download   []Behavior       `json:"download,omitempty"`
	Subs       []Behavior       `json:"subs,omitempty"`
}

// Playlist is a fake playlist of video ids.
type Playlist struct {
	ID      string     `json:"id"`
	Title   string     `json:"title"`
	Entries []string   `json:"entries"`
	Probe   []Behavior `json:"probe,omitempty"`
}

// Scenario is the whole fake world.
type Scenario struct {
	Version   string              `json:"version"`
	Targets   []string            `json:"targets"` // available impersonation targets "Client OS"
	Videos    map[string]Video    `json:"videos"`
	Playlists map[string]Playlist `json:"playlists"`
}

// URL conventions of the fake site.
const (
	VideoPrefix    = "https://fake.test/v/"
	PlaylistPrefix = "https://fake.test/pl/"
)

// MediaInfo is written into fake media files; the fake ffprobe reads it.
type MediaInfo struct {
	FakeMedia bool    `json:"fake_media"`
	Duration  float64 `json:"duration"`
	Video     int     `json:"video"`
	Audio     int     `json:"audio"`
	Pad       string  `json:"pad,omitempty"`
}

// Write stores a scenario file.
func (s *Scenario) Write(path string) error {
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Load reads a scenario file.
func Load(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	return &s, json.Unmarshal(b, &s)
}
