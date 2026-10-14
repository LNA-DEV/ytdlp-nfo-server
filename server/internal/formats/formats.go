// Package formats selects yt-dlp formats (port of discover_audio_formats).
package formats

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AudioTrack is the best audio-only format for one (language, channels).
type AudioTrack struct {
	Language string
	Channels int
	FormatID string
	ABR      float64
}

// Label returns e.g. "en (2ch)".
func (a AudioTrack) Label() string { return fmt.Sprintf("%s (%dch)", a.Language, a.Channels) }

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f
	case int:
		return float64(x)
	}
	return 0
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// codec mirrors Python's f.get(key, "none"): a missing key counts as "none",
// an explicit null does not.
func codec(f map[string]any, key string) (string, bool) {
	v, ok := f[key]
	if !ok {
		return "none", true
	}
	if v == nil {
		return "", false
	}
	return str(v), true
}

// SelectAudioTracks returns the best audio-only format per
// (language, channels), keeping both surround and stereo per language.
// The rule and the tie-breaking (first format wins on equal bitrate) are
// the same as in ytdlp-nfo.
func SelectAudioTracks(formats []map[string]any) []AudioTrack {
	type key struct {
		lang string
		ch   int
	}
	var order []key
	best := map[key]AudioTrack{}
	for _, f := range formats {
		lang, fid := str(f["language"]), str(f["format_id"])
		if lang == "" || fid == "" {
			continue
		}
		ac, acKnown := codec(f, "acodec")
		vc, vcKnown := codec(f, "vcodec")
		isAudio := !acKnown || ac != "none" // null acodec counts as audio
		isAudioOnly := vcKnown && vc == "none"
		if !isAudio || !isAudioOnly {
			continue
		}
		ch := int(num(f["audio_channels"]))
		if ch == 0 {
			ch = 2
		}
		abr := num(f["abr"])
		k := key{lang, ch}
		cur, seen := best[k]
		if !seen {
			order = append(order, k)
		}
		if !seen || abr > cur.ABR {
			best[k] = AudioTrack{Language: lang, Channels: ch, FormatID: fid, ABR: abr}
		}
	}
	out := make([]AudioTrack, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

// Selection is the format part of a download command.
type Selection struct {
	Format           string // -f
	AudioMultistream bool   // --audio-multistreams
	Sort             string // -S, empty for none
	Tracks           []AudioTrack
}

// Select builds the format selection for a video. With allAudio and more
// than one audio track it requests every track: "bv+a1+a2/bv*+ba/best".
func Select(formats []map[string]any, allAudio bool, sort string) Selection {
	sel := Selection{Format: "bv*+ba/best", Sort: sort}
	if !allAudio {
		return sel
	}
	tracks := SelectAudioTracks(formats)
	if len(tracks) > 1 {
		ids := make([]string, len(tracks))
		for i, t := range tracks {
			ids[i] = t.FormatID
		}
		sel.Format = "bv+" + strings.Join(ids, "+") + "/bv*+ba/best"
		sel.AudioMultistream = true
		sel.Tracks = tracks
	}
	return sel
}

// Components describes the media streams of a chosen format_id such as
// "137+251-0+251-1", looked up in the info's formats list.
type Components struct {
	Video int // components carrying video
	Audio int // components carrying audio
	Known bool
}

// Describe counts video and audio components of a (possibly merged)
// format id. Unknown codecs count as present so verification stays lenient.
func Describe(formats []map[string]any, formatID string) Components {
	byID := map[string]map[string]any{}
	for _, f := range formats {
		if id := str(f["format_id"]); id != "" {
			byID[id] = f
		}
	}
	var c Components
	if formatID == "" {
		return c
	}
	c.Known = true
	for _, id := range strings.Split(formatID, "+") {
		f, ok := byID[id]
		if !ok {
			c.Known = false
			continue
		}
		vc, vcKnown := codec(f, "vcodec")
		ac, acKnown := codec(f, "acodec")
		if !vcKnown || vc != "none" {
			c.Video++
		}
		if !acKnown || ac != "none" {
			c.Audio++
		}
	}
	return c
}

// AudioOnlySource reports whether no format of the video carries video,
// e.g. podcasts or music. Verification then does not require a video stream.
func AudioOnlySource(formats []map[string]any) bool {
	if len(formats) == 0 {
		return false
	}
	for _, f := range formats {
		vc, known := codec(f, "vcodec")
		if !known || vc != "none" {
			return false
		}
	}
	return true
}
