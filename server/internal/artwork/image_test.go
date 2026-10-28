package artwork

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color/palette"
	"image/gif"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyImage(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var anim bytes.Buffer
	frame := image.NewPaletted(image.Rect(0, 0, 10, 6), palette.Plan9)
	gif.EncodeAll(&anim, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{5, 5}})
	// A WebP is probed with ffprobe; this stand-in reports one stream.
	probe := write("ffprobe", []byte("#!/bin/sh\necho '{\"streams\":[{\"codec_type\":\"video\",\"width\":640,\"height\":480}]}'\n"))
	os.Chmod(probe, 0o755)
	png := encode(t, TypePNG)

	ctx := context.Background()
	for _, c := range []struct {
		name string
		data []byte
		want ImageInfo
		err  string
	}{
		{"a.jpg", encode(t, TypeJPEG), ImageInfo{TypeJPEG, 16, 9}, ""},
		{"a.png", png, ImageInfo{TypePNG, 16, 9}, ""},
		{"anim.gif", anim.Bytes(), ImageInfo{TypeGIF, 10, 6}, ""},
		{"a.webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 data"), ImageInfo{TypeWebP, 640, 480}, ""},
		{"page.png", []byte("<!doctype html><title>Imgur</title>"), ImageInfo{}, "not an image"},
		{"cut.png", png[:len(png)/2], ImageInfo{}, "broken png"},
		{"cut.gif", anim.Bytes()[:40], ImageInfo{}, "broken GIF"},
	} {
		info, err := VerifyImage(ctx, write(c.name, c.data), probe)
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.err == "" && info != c.want:
			t.Errorf("%s: %+v, want %+v", c.name, info, c.want)
		case c.err != "" && (err == nil || !bytes.Contains([]byte(err.Error()), []byte(c.err))):
			t.Errorf("%s: err %v, want %q", c.name, err, c.err)
		}
	}
	if _, err := VerifyImage(ctx, write("x.html", []byte("<html>")), probe); !errors.Is(err, ErrNotImage) {
		t.Fatalf("html: %v", err)
	}
	if (ImageInfo{Type: TypeJPEG}).Ext() != "jpg" || (ImageInfo{Type: TypeWebP}).Ext() != "webp" {
		t.Fatal("ext")
	}
}
