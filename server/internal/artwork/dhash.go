package artwork

import (
	"context"
	"fmt"
	"image"
	"math"
	"math/bits"
	"os"
	"path/filepath"
)

// Level 3 of the duplicate check (Reddit concept §7.5): a 64-bit difference
// hash. The image is scaled to 9 × 8 gray cells, and each bit says whether
// a cell is brighter than its right neighbour, so resizing and
// recompression barely change it.

const (
	// minHashSide and minHashStdDev exclude small and almost uniform
	// images: their hashes match too easily.
	minHashSide   = 64
	minHashStdDev = 3.0 // of the 72 cells, on a 0–255 scale
	// maxHashSamples bounds the pixels read per axis of a large image.
	maxHashSamples = 512
)

// DHash returns the difference hash of an image; ok is false for images
// that get no hash.
func DHash(img image.Image) (hash uint64, ok bool) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < minHashSide || h < minHashSide {
		return 0, false
	}
	var sum, n [72]float64
	stepX, stepY := max(1, w/maxHashSamples), max(1, h/maxHashSamples)
	for y := b.Min.Y; y < b.Max.Y; y += stepY {
		row := (y - b.Min.Y) * 8 / h * 9
		for x := b.Min.X; x < b.Max.X; x += stepX {
			r, g, bl, _ := img.At(x, y).RGBA()
			c := row + (x-b.Min.X)*9/w
			sum[c] += (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 257
			n[c]++
		}
	}
	var cells [72]float64
	for i := range cells {
		if n[i] > 0 {
			cells[i] = sum[i] / n[i]
		}
	}
	return hashCells(cells)
}

func hashCells(cells [72]float64) (uint64, bool) {
	var mean, sq float64
	for _, v := range cells {
		mean += v
	}
	mean /= 72
	for _, v := range cells {
		sq += (v - mean) * (v - mean)
	}
	if math.Sqrt(sq/72) < minHashStdDev {
		return 0, false
	}
	var hash uint64
	for row := 0; row < 8; row++ {
		for col := 0; col < 8; col++ {
			if cells[row*9+col] > cells[row*9+col+1] {
				hash |= 1 << (row*8 + col)
			}
		}
	}
	return hash, true
}

// Distance returns the number of bits two hashes differ in.
func Distance(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// DHashFile hashes an image the standard library can't decode (WebP,
// AVIF): ffmpeg scales its first frame to 9 × 8 gray pixels.
func (m *Maker) DHashFile(ctx context.Context, path string, info ImageInfo) (uint64, bool, error) {
	if info.Width < minHashSide || info.Height < minHashSide {
		return 0, false, nil
	}
	out := filepath.Join(filepath.Dir(path), ".dhash.gray")
	defer os.Remove(out)
	if err := m.ffmpeg(ctx, "-i", path, "-frames:v", "1", "-vf", "scale=9:8:flags=area,format=gray", "-f", "rawvideo", out); err != nil {
		return 0, false, err
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return 0, false, err
	}
	if len(raw) != 72 {
		return 0, false, fmt.Errorf("ffmpeg returned %d bytes instead of 9×8 pixels", len(raw))
	}
	var cells [72]float64
	for i, v := range raw {
		cells[i] = float64(v)
	}
	hash, ok := hashCells(cells)
	return hash, ok, nil
}
