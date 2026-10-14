// Package artwork produces folder.jpg for an item (§4.8). The image type is
// detected from the content, never from the URL, so CDN suffixes such as
// ".v1712345678" do not matter and yt-dlp's extension allowlist is never
// involved (F21). If no thumbnail works, a frame of the media is used.
package artwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/procutil"
)

// Image types detected by Sniff.
const (
	TypeJPEG = "jpeg"
	TypePNG  = "png"
	TypeGIF  = "gif"
	TypeWebP = "webp"
	TypeAVIF = "avif"
)

// Sniff detects the image type from magic bytes.
func Sniff(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return TypeJPEG
	case len(b) >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		return TypePNG
	case len(b) >= 6 && (bytes.Equal(b[:6], []byte("GIF87a")) || bytes.Equal(b[:6], []byte("GIF89a"))):
		return TypeGIF
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return TypeWebP
	case len(b) >= 12 && bytes.Equal(b[4:8], []byte("ftyp")) && (bytes.Equal(b[8:12], []byte("avif")) || bytes.Equal(b[8:12], []byte("avis"))):
		return TypeAVIF
	}
	return ""
}

// Thumbnail is a candidate from info.json.
type Thumbnail struct {
	URL        string
	Preference float64
	Pixels     float64
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

// Candidates orders info.json thumbnails by preference and resolution,
// plus the top-level thumbnail, without duplicates.
func Candidates(thumbs []map[string]any, top string) []string {
	var list []Thumbnail
	for i, t := range thumbs {
		u, _ := t["url"].(string)
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			continue
		}
		pref := num(t["preference"])
		if _, ok := t["preference"]; !ok {
			pref = float64(i) // yt-dlp lists thumbnails worst to best
		}
		list = append(list, Thumbnail{URL: u, Preference: pref, Pixels: num(t["width"]) * num(t["height"])})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Preference != list[j].Preference {
			return list[i].Preference > list[j].Preference
		}
		return list[i].Pixels > list[j].Pixels
	})
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		if u != "" && !seen[u] && (strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) {
			seen[u] = true
			out = append(out, u)
		}
	}
	add(top)
	for _, t := range list {
		add(t.URL)
	}
	return out
}

// Maker creates folder.jpg files.
type Maker struct {
	Client        *http.Client
	FFmpeg        string
	MaxBytes      int64
	Timeout       time.Duration
	MaxCandidates int
}

func (m *Maker) maxBytes() int64 {
	if m.MaxBytes > 0 {
		return m.MaxBytes
	}
	return 20 << 20
}

func (m *Maker) client() *http.Client {
	if m.Client != nil {
		return m.Client
	}
	return http.DefaultClient
}

// Request describes what to produce.
type Request struct {
	Candidates []string
	Headers    map[string]string
	MediaPath  string  // verified media for the frame fallback
	Duration   float64 // seconds
	HasVideo   bool
	Dst        string // e.g. <work>/folder.jpg
}

// Result reports where the image came from and why candidates failed.
type Result struct {
	Source   string // "thumbnail" or "frame"
	Attempts []string
}

// ErrNoThumbnail means neither a thumbnail nor a frame could be produced.
var ErrNoThumbnail = errors.New("no thumbnail")

// Make writes a valid JPEG to req.Dst.
func (m *Maker) Make(ctx context.Context, req Request) (Result, error) {
	var res Result
	n := m.MaxCandidates
	if n <= 0 {
		n = 3
	}
	for i, u := range req.Candidates {
		if i >= n {
			break
		}
		err := m.fromURL(ctx, u, req)
		if err == nil {
			res.Source = "thumbnail"
			return res, nil
		}
		res.Attempts = append(res.Attempts, fmt.Sprintf("thumbnail %d: %v", i+1, err))
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
	}
	if req.MediaPath != "" && req.HasVideo {
		if err := m.fromFrame(ctx, req); err == nil {
			res.Source = "frame"
			return res, nil
		} else {
			res.Attempts = append(res.Attempts, "frame: "+err.Error())
		}
	}
	return res, ErrNoThumbnail
}

func (m *Maker) fromURL(ctx context.Context, u string, req Request) error {
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	hreq.Header.Del("Accept-Encoding") // let Go handle gzip transparently
	resp, err := m.client().Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, m.maxBytes()+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > m.maxBytes() {
		return fmt.Errorf("larger than %d bytes", m.maxBytes())
	}
	kind := Sniff(data)
	if kind == "" {
		return fmt.Errorf("content is not a supported image (%s)", strings.TrimSpace(resp.Header.Get("Content-Type")))
	}
	return m.writeJPEG(ctx, data, kind, req.Dst)
}

// writeJPEG converts data to JPEG if needed, validates it by decoding and
// writes it to dst.
func (m *Maker) writeJPEG(ctx context.Context, data []byte, kind, dst string) error {
	var out []byte
	switch kind {
	case TypeJPEG:
		out = data
	case TypePNG, TypeGIF:
		var img image.Image
		var err error
		if kind == TypePNG {
			img, err = png.Decode(bytes.NewReader(data))
		} else {
			img, err = gif.Decode(bytes.NewReader(data))
		}
		if err != nil {
			return fmt.Errorf("decode %s: %w", kind, err)
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
			return err
		}
		out = buf.Bytes()
	default: // webp, avif: ffmpeg
		dir := filepath.Dir(dst)
		in := filepath.Join(dir, ".thumb-src."+kind)
		conv := filepath.Join(dir, ".thumb-conv.jpg")
		defer os.Remove(in)
		defer os.Remove(conv)
		if err := os.WriteFile(in, data, 0o644); err != nil {
			return err
		}
		if err := m.ffmpeg(ctx, "-i", in, "-frames:v", "1", "-q:v", "2", conv); err != nil {
			return fmt.Errorf("convert %s: %w", kind, err)
		}
		b, err := os.ReadFile(conv)
		if err != nil {
			return err
		}
		out = b
	}
	if err := ValidateJPEG(out); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ValidateJPEG decodes the image once to confirm it is a usable JPEG.
func ValidateJPEG(b []byte) error {
	if Sniff(b) != TypeJPEG {
		return errors.New("result is not a JPEG")
	}
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("invalid JPEG: %w", err)
	}
	if r := img.Bounds(); r.Dx() == 0 || r.Dy() == 0 {
		return errors.New("empty image")
	}
	return nil
}

func (m *Maker) fromFrame(ctx context.Context, req Request) error {
	at := req.Duration * 0.1
	tmp := filepath.Join(filepath.Dir(req.Dst), ".frame.jpg")
	defer os.Remove(tmp)
	args := []string{"-ss", strconv.FormatFloat(at, 'f', 3, 64), "-i", req.MediaPath, "-frames:v", "1", "-q:v", "2", tmp}
	if err := m.ffmpeg(ctx, args...); err != nil {
		return err
	}
	b, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	if err := ValidateJPEG(b); err != nil {
		return err
	}
	return os.Rename(tmp, req.Dst)
}

func (m *Maker) ffmpeg(ctx context.Context, args ...string) error {
	bin := m.FFmpeg
	if bin == "" {
		bin = "ffmpeg"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	full := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, args...)
	cmd := exec.Command(bin, full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := procutil.Run(ctx, cmd, 5*time.Second); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return fmt.Errorf("ffmpeg: %v %s", err, msg)
	}
	return nil
}
