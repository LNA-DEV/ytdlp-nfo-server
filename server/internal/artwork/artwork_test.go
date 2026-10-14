package artwork

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func testImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 16, 9))
	for x := 0; x < 16; x++ {
		img.Set(x, 4, color.RGBA{R: 200, A: 255})
	}
	return img
}

func encode(t *testing.T, kind string) []byte {
	var buf bytes.Buffer
	switch kind {
	case TypeJPEG:
		jpeg.Encode(&buf, testImage(), nil)
	case TypePNG:
		png.Encode(&buf, testImage())
	}
	return buf.Bytes()
}

func TestSniff(t *testing.T) {
	cases := map[string][]byte{
		TypeJPEG: encode(t, TypeJPEG),
		TypePNG:  encode(t, TypePNG),
		TypeGIF:  []byte("GIF89a...."),
		TypeWebP: []byte("RIFF\x00\x00\x00\x00WEBPVP8 "),
		TypeAVIF: []byte("\x00\x00\x00\x1cftypavif"),
		"":       []byte("<!doctype html>"),
	}
	for want, b := range cases {
		if got := Sniff(b); got != want {
			t.Errorf("Sniff = %q, want %q", got, want)
		}
	}
}

func TestCandidatesOrder(t *testing.T) {
	thumbs := []map[string]any{
		{"url": "https://cdn/a.jpg", "preference": -10.0, "width": 1280.0, "height": 720.0},
		{"url": "https://cdn/b.jpg", "preference": 0.0, "width": 120.0, "height": 90.0},
		{"url": "https://cdn/c.jpg", "preference": 0.0, "width": 640.0, "height": 360.0},
		{"url": "data:image/png;base64,xx"},
	}
	got := Candidates(thumbs, "https://cdn/c.jpg")
	want := []string{"https://cdn/c.jpg", "https://cdn/b.jpg", "https://cdn/a.jpg"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A CDN URL with a version suffix (".v<timestamp>") that serves a PNG must
// still produce folder.jpg: the type comes from the content (F21).
func TestURLSuffixDoesNotMatter(t *testing.T) {
	pngData := encode(t, TypePNG)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/html.jpg":
			w.Write([]byte("<html>blocked</html>"))
		case "/thumb.v1712345678":
			if r.Header.Get("User-Agent") != "TestUA" {
				http.Error(w, "no ua", 403)
				return
			}
			w.Write(pngData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "folder.jpg")
	m := &Maker{}
	res, err := m.Make(context.Background(), Request{
		Candidates: []string{srv.URL + "/missing", srv.URL + "/html.jpg", srv.URL + "/thumb.v1712345678"},
		Headers:    map[string]string{"User-Agent": "TestUA"},
		Dst:        dst,
	})
	if err != nil {
		t.Fatalf("%v (%v)", err, res.Attempts)
	}
	if len(res.Attempts) != 2 || res.Source != "thumbnail" {
		t.Fatalf("res = %+v", res)
	}
	b, _ := os.ReadFile(dst)
	if err := ValidateJPEG(b); err != nil {
		t.Fatal(err)
	}
}

func TestFrameFallbackAndFailure(t *testing.T) {
	dir := t.TempDir()
	jpg := filepath.Join(dir, "frame-src.jpg")
	os.WriteFile(jpg, encode(t, TypeJPEG), 0o644)
	ff := filepath.Join(dir, "ffmpeg")
	// Fake ffmpeg: copy a JPEG to the last argument.
	os.WriteFile(ff, []byte("#!/bin/sh\nfor last; do :; done\ncp "+jpg+" \"$last\"\n"), 0o755)
	m := &Maker{FFmpeg: ff}
	dst := filepath.Join(dir, "folder.jpg")
	res, err := m.Make(context.Background(), Request{Candidates: nil, MediaPath: "/x/media.mkv", Duration: 100, HasVideo: true, Dst: dst})
	if err != nil || res.Source != "frame" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// Audio-only and no candidates: a warning, not a crash.
	os.Remove(dst)
	if _, err := m.Make(context.Background(), Request{MediaPath: "/x/a.opus", HasVideo: false, Dst: dst}); err != ErrNoThumbnail {
		t.Fatalf("err = %v", err)
	}
}
