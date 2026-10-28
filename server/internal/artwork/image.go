package artwork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/procutil"
)

// ErrNotImage means the content is not an image, for example an HTML
// error page served with status 200.
var ErrNotImage = errors.New("not an image")

// ImageInfo describes a verified image.
type ImageInfo struct {
	Type   string // jpeg, png, gif, webp or avif
	Width  int
	Height int
	// DHash is the difference hash if Hashed (JPEG, PNG and GIF; WebP and
	// AVIF are hashed with Maker.DHashFile).
	DHash  uint64
	Hashed bool
}

// Ext returns the file extension of the image type.
func (i ImageInfo) Ext() string {
	if i.Type == TypeJPEG {
		return "jpg"
	}
	return i.Type
}

// maxGIFDecodeAll bounds the GIFs whose frames are all decoded; larger
// ones are checked by their first frame, so a long animation can't take
// the memory of every frame at once.
const maxGIFDecodeAll = 16 << 20

// VerifyImage checks an image by its content, never by its name (Reddit
// concept §7.3): JPEG, PNG and GIF are decoded with the standard library,
// WebP and AVIF are probed with ffprobe. Width and height must be > 0.
func VerifyImage(ctx context.Context, path, ffprobe string) (ImageInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return ImageInfo{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ImageInfo{}, err
	}
	head := make([]byte, 64)
	n, _ := io.ReadFull(f, head)
	info := ImageInfo{Type: Sniff(head[:n])}
	if info.Type == "" {
		return info, ErrNotImage
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return info, err
	}
	switch info.Type {
	case TypeGIF:
		if fi.Size() <= maxGIFDecodeAll {
			g, err := gif.DecodeAll(f)
			if err != nil {
				return info, fmt.Errorf("broken GIF: %w", err)
			}
			info.Width, info.Height = g.Config.Width, g.Config.Height
			if len(g.Image) == 0 {
				return info, errors.New("GIF without frames")
			}
			info.DHash, info.Hashed = DHash(g.Image[0])
			break
		}
		fallthrough
	case TypeJPEG, TypePNG:
		img, _, err := image.Decode(f)
		if err != nil {
			return info, fmt.Errorf("broken %s: %w", info.Type, err)
		}
		info.Width, info.Height = img.Bounds().Dx(), img.Bounds().Dy()
		info.DHash, info.Hashed = DHash(img)
	default:
		if err := probeImage(ctx, path, ffprobe, &info); err != nil {
			return info, err
		}
	}
	if info.Width <= 0 || info.Height <= 0 {
		return info, errors.New("image has no size")
	}
	return info, nil
}

func probeImage(ctx context.Context, path, ffprobe string, info *ImageInfo) error {
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.Command(ffprobe, "-v", "error", "-print_format", "json", "-show_streams", path)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := procutil.Run(ctx, cmd, 5*time.Second); err != nil {
		return fmt.Errorf("broken %s: ffprobe: %v %s", info.Type, err, bytes.TrimSpace(stderr.Bytes()))
	}
	var probe struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out.Bytes(), &probe); err != nil {
		return fmt.Errorf("ffprobe output: %v", err)
	}
	for _, s := range probe.Streams {
		if s.CodecType == "video" {
			info.Width, info.Height = s.Width, s.Height
			return nil
		}
	}
	return fmt.Errorf("broken %s: no image stream", info.Type)
}

// FromBytes writes an image, converted to JPEG if needed, to dst (a
// thumbnail fetched by the caller).
func (m *Maker) FromBytes(ctx context.Context, data []byte, dst string) error {
	kind := Sniff(data)
	if kind == "" {
		return ErrNotImage
	}
	return m.writeJPEG(ctx, data, kind, dst)
}
