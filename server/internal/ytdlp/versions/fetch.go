// Package versions manages yt-dlp standalone builds: channel listing,
// verified install, smoke test, activation, retention and auto-update (§6).
package versions

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
)

// Repos per channel (§6.1).
var Repos = map[string]string{
	config.ChannelStable:  "yt-dlp/yt-dlp",
	config.ChannelNightly: "yt-dlp/yt-dlp-nightly-builds",
	config.ChannelMaster:  "yt-dlp/yt-dlp-master-builds",
}

const (
	maxAssetSize = 400 << 20
	sumsAsset    = "SHA2-256SUMS"
	manifestName = "manifest.json"
)

// Assets returns the unpacked (onedir .zip) and onefile asset names for a
// GOARCH. The zip variant starts faster; onefile is the fallback.
func Assets(arch string) (zipAsset, onefile string, err error) {
	switch arch {
	case "amd64":
		return "yt-dlp_linux.zip", "yt-dlp_linux", nil
	case "arm64":
		return "yt-dlp_linux_aarch64.zip", "yt-dlp_linux_aarch64", nil
	case "arm":
		return "yt-dlp_linux_armv7l.zip", "", nil
	}
	return "", "", fmt.Errorf("no yt-dlp build for architecture %s", arch)
}

// Manifest describes an unpacked build on disk.
type Manifest struct {
	Channel string `json:"channel"`
	Tag     string `json:"tag"`
	Asset   string `json:"asset"`
	SHA256  string `json:"sha256"`
	Exe     string `json:"exe"` // path relative to the build dir
}

// Fetcher downloads and verifies release assets.
type Fetcher struct {
	Client      *http.Client
	DownloadURL string // default https://github.com
	Token       string
}

func (f *Fetcher) base() string {
	if f.DownloadURL != "" {
		return strings.TrimRight(f.DownloadURL, "/")
	}
	return "https://github.com"
}

func (f *Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return http.DefaultClient
}

func (f *Fetcher) assetURL(channel, tag, asset string) (string, error) {
	repo, ok := Repos[channel]
	if !ok {
		return "", fmt.Errorf("unknown channel %q", channel)
	}
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", f.base(), repo, tag, asset), nil
}

func (f *Fetcher) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ytdlp-nfo-server")
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// Sums downloads SHA2-256SUMS of a release.
func (f *Fetcher) Sums(ctx context.Context, channel, tag string) (map[string]string, error) {
	u, err := f.assetURL(channel, tag, sumsAsset)
	if err != nil {
		return nil, err
	}
	resp, err := f.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	sums := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && len(fields[0]) == 64 {
			sums[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
		}
	}
	if len(sums) == 0 {
		return nil, fmt.Errorf("%s of %s %s is empty", sumsAsset, channel, tag)
	}
	return sums, sc.Err()
}

// download saves an asset to dst and verifies its SHA-256.
func (f *Fetcher) download(ctx context.Context, channel, tag, asset, want, dst string) error {
	u, err := f.assetURL(channel, tag, asset)
	if err != nil {
		return err
	}
	resp, err := f.get(ctx, u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, maxAssetSize+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > maxAssetSize {
		return fmt.Errorf("%s is larger than %d bytes", asset, maxAssetSize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("SHA-256 mismatch for %s: got %s, want %s", asset, got, want)
	}
	return nil
}

// Fetch downloads, verifies and unpacks a build into destDir, which must
// not exist yet. It writes manifest.json last.
func (f *Fetcher) Fetch(ctx context.Context, channel, tag, arch, destDir string) (*Manifest, error) {
	if !validTag(tag) {
		return nil, fmt.Errorf("invalid tag %q", tag)
	}
	zipAsset, onefile, err := Assets(arch)
	if err != nil {
		return nil, err
	}
	sums, err := f.Sums(ctx, channel, tag)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manifest{Channel: channel, Tag: tag}
	if want, ok := sums[zipAsset]; ok {
		tmp := filepath.Join(destDir, ".download.zip")
		if err := f.download(ctx, channel, tag, zipAsset, want, tmp); err != nil {
			return nil, err
		}
		exe, err := unzip(tmp, destDir, strings.TrimSuffix(zipAsset, ".zip"))
		os.Remove(tmp)
		if err != nil {
			return nil, fmt.Errorf("unpack %s: %w", zipAsset, err)
		}
		m.Asset, m.SHA256, m.Exe = zipAsset, want, exe
	} else if want, ok := sums[onefile]; ok && onefile != "" {
		dst := filepath.Join(destDir, onefile)
		if err := f.download(ctx, channel, tag, onefile, want, dst); err != nil {
			return nil, err
		}
		if err := os.Chmod(dst, 0o755); err != nil {
			return nil, err
		}
		m.Asset, m.SHA256, m.Exe = onefile, want, onefile
	} else {
		return nil, fmt.Errorf("release %s %s has no build for %s", channel, tag, arch)
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(destDir, manifestName), data, 0o644); err != nil {
		return nil, err
	}
	return m, nil
}

// unzip extracts a PyInstaller onedir zip safely (no path traversal, no
// symlinks) and returns the executable's relative path.
func unzip(src, dest, exeName string) (string, error) {
	r, err := zip.OpenReader(src)
	if err != nil {
		return "", err
	}
	defer r.Close()
	exe := ""
	var total int64
	for _, zf := range r.File {
		name := filepath.FromSlash(zf.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(filepath.Clean(name), "..") || strings.Contains(zf.Name, "\\") {
			return "", fmt.Errorf("unsafe path %q in archive", zf.Name)
		}
		target := filepath.Join(dest, name)
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if zf.Mode()&os.ModeSymlink != 0 || !zf.Mode().IsRegular() && zf.Mode() != 0 {
			return "", fmt.Errorf("unsupported entry %q in archive", zf.Name)
		}
		total += int64(zf.UncompressedSize64)
		if total > 2*maxAssetSize {
			return "", errors.New("archive too large")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		mode := os.FileMode(0o644)
		if zf.Mode()&0o111 != 0 || filepath.Base(name) == exeName {
			mode = 0o755
		}
		rc, err := zf.Open()
		if err != nil {
			return "", err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			rc.Close()
			return "", err
		}
		_, err = io.Copy(out, io.LimitReader(rc, int64(zf.UncompressedSize64)+1))
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
		if filepath.Dir(name) == "." && filepath.Base(name) == exeName {
			exe = name
		}
	}
	if exe == "" {
		return "", fmt.Errorf("executable %s not found in archive", exeName)
	}
	return exe, nil
}

// ReadManifest reads manifest.json of an unpacked build.
func ReadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Exe == "" || strings.Contains(m.Exe, "..") {
		return nil, fmt.Errorf("invalid manifest in %s", dir)
	}
	return &m, nil
}

func validTag(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	for _, c := range t {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// CompareTags compares release tags numerically ("2026.10.10.233141").
func CompareTags(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int64
		var xs, ys string
		if i < len(pa) {
			xs = pa[i]
			x, _ = strconv.ParseInt(xs, 10, 64)
		}
		if i < len(pb) {
			ys = pb[i]
			y, _ = strconv.ParseInt(ys, 10, 64)
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
		if xs != ys {
			return strings.Compare(xs, ys)
		}
	}
	return 0
}

// LatestTag resolves the newest tag of a channel via the
// releases/latest redirect (no API rate limit).
func (f *Fetcher) LatestTag(ctx context.Context, channel string) (string, error) {
	repo, ok := Repos[channel]
	if !ok {
		return "", fmt.Errorf("unknown channel %q", channel)
	}
	url := fmt.Sprintf("%s/%s/releases/latest", f.base(), repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ytdlp-nfo-server")
	c := *f.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", fmt.Errorf("unexpected redirect %q (%s)", loc, resp.Status)
	}
	tag := loc[i+len("/tag/"):]
	if !validTag(tag) {
		return "", fmt.Errorf("invalid tag %q", tag)
	}
	return tag, nil
}
