package artwork

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand/v2"
	"testing"
)

// blocks is a 12 × 9 grid of random gray-ish colors, scaled to w × h.
func blocks(seed uint64, w, h int) *image.RGBA {
	r := rand.New(rand.NewPCG(seed, 1))
	var grid [9][12]color.RGBA
	for y := range grid {
		for x := range grid[y] {
			grid[y][x] = color.RGBA{uint8(r.IntN(256)), uint8(r.IntN(256)), uint8(r.IntN(256)), 255}
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, grid[y*9/h][x*12/w])
		}
	}
	return img
}

// shrink scales an image down by averaging (a repost resized by a site).
func shrink(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, bl, n uint32
			for sy := y * b.Dy() / h; sy < (y+1)*b.Dy()/h; sy++ {
				for sx := x * b.Dx() / w; sx < (x+1)*b.Dx()/w; sx++ {
					cr, cg, cb, _ := src.At(sx, sy).RGBA()
					r, g, bl, n = r+cr>>8, g+cg>>8, bl+cb>>8, n+1
				}
			}
			dst.Set(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), 255})
		}
	}
	return dst
}

func TestDHash(t *testing.T) {
	orig := blocks(1, 640, 480)
	h1, ok := DHash(orig)
	if !ok {
		t.Fatal("no hash")
	}
	// A resized, recompressed repost is similar.
	var buf bytes.Buffer
	jpeg.Encode(&buf, shrink(orig, 320, 240), &jpeg.Options{Quality: 60})
	repost, err := jpeg.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	h2, ok := DHash(repost)
	if d := Distance(h1, h2); !ok || d > 6 {
		t.Fatalf("repost distance %d (ok %v)", d, ok)
	}
	// Other pictures are not.
	for seed := uint64(2); seed < 20; seed++ {
		h, _ := DHash(blocks(seed, 640, 480))
		if d := Distance(h1, h); d <= 6 {
			t.Fatalf("picture %d: distance %d", seed, d)
		}
	}
	// Small and almost uniform images get no hash.
	if _, ok := DHash(blocks(1, 60, 200)); ok {
		t.Fatal("small image hashed")
	}
	gray := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for i := range gray.Pix {
		gray.Pix[i] = 128
	}
	gray.Set(5, 5, color.White)
	if _, ok := DHash(gray); ok {
		t.Fatal("uniform image hashed")
	}
}
