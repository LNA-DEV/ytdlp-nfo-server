package library

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MarkerName is the hidden file in every Reddit item folder (Reddit
// concept §8). Image folders have no movie.nfo; the marker lets target
// selection and the library check recognize them.
const MarkerName = ".ytdlp-nfo.json"

// Marker describes a Reddit item folder.
type Marker struct {
	Key       string        `json:"key"` // archive key, e.g. "redditgallery 5f2ab1c09d3e4a7b"
	Post      string        `json:"post"`
	Kind      string        `json:"kind"`
	Subreddit string        `json:"subreddit,omitempty"`
	Author    string        `json:"author,omitempty"`
	Title     string        `json:"title,omitempty"`
	Permalink string        `json:"permalink,omitempty"`
	Created   time.Time     `json:"created"`
	Provider  string        `json:"provider,omitempty"`
	Files     []MarkerFile  `json:"files"`
	Known     []MarkerKnown `json:"known,omitempty"`
}

// MarkerFile is a media file of the folder.
type MarkerFile struct {
	Name   string `json:"name"`
	Media  string `json:"media"` // media key
	SHA256 string `json:"sha256,omitempty"`
}

// MarkerKnown is an image of a gallery that was already in the library
// and was not stored again: Path is the file that has it, relative to the
// library root. SHA256 is set when the image was found identical after
// the download (a new media key, recorded as an alias).
type MarkerKnown struct {
	Position int    `json:"position"`
	Media    string `json:"media"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256,omitempty"`
}

// ID returns the id part of the marker's key.
func (m *Marker) ID() string {
	_, id, _ := strings.Cut(m.Key, " ")
	return id
}

// Encode returns the marker as JSON.
func (m *Marker) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ReadMarker reads the marker of a folder; nil if there is none.
func ReadMarker(dir string) *Marker {
	f, err := os.Open(filepath.Join(dir, MarkerName))
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return nil
	}
	var m Marker
	if json.Unmarshal(data, &m) != nil || m.Key == "" {
		return nil
	}
	return &m
}
