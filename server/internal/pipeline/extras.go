package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/artwork"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/procutil"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

var timeZero time.Time

// VerifyResult describes the verified media file.
type VerifyResult struct {
	Duration float64
	Video    int
	Audio    int
	Size     int64
}

// verify checks the media with ffprobe (§4.4): parseable, duration > 0, a
// video stream unless audio-only, and the expected number of audio streams.
func (p *Pipeline) verify(ctx context.Context, path string, expectVideo bool, expectAudio int) (VerifyResult, error) {
	var vr VerifyResult
	fi, err := os.Stat(path)
	if err != nil {
		return vr, err
	}
	if fi.Size() == 0 {
		return vr, fmt.Errorf("file is empty")
	}
	vr.Size = fi.Size()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.Command(p.d.Env.FFprobePath, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := procutil.Run(ctx, cmd, 5*time.Second); err != nil {
		return vr, fmt.Errorf("ffprobe: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	var probe struct {
		Streams []struct {
			CodecType   string         `json:"codec_type"`
			Duration    string         `json:"duration"`
			Disposition map[string]int `json:"disposition"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out.Bytes(), &probe); err != nil {
		return vr, fmt.Errorf("ffprobe output: %v", err)
	}
	vr.Duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video":
			if s.Disposition["attached_pic"] == 0 {
				vr.Video++
			}
		case "audio":
			vr.Audio++
		}
		if d, err := strconv.ParseFloat(s.Duration, 64); err == nil && d > vr.Duration {
			vr.Duration = d
		}
	}
	switch {
	case vr.Duration <= 0:
		return vr, fmt.Errorf("media has no duration")
	case expectVideo && vr.Video == 0:
		return vr, fmt.Errorf("no video stream")
	case vr.Audio < expectAudio:
		return vr, fmt.Errorf("expected %d audio stream(s), found %d", expectAudio, vr.Audio)
	}
	return vr, nil
}

// extras fetches the thumbnail and subtitles. Failures become warnings,
// never item failures (§4.8, F21).
func (p *Pipeline) extras(ctx context.Context, rc *runCtx, it *store.Item, meta *ytdlp.Meta, dir, mediaPath string, vr VerifyResult) []subFile {
	// Thumbnail -> folder.jpg
	res, err := p.d.Artwork.Make(ctx, artwork.Request{
		Candidates: artwork.Candidates(meta.Thumbnails, meta.Thumbnail),
		Headers:    meta.HTTPHeaders,
		MediaPath:  mediaPath,
		Duration:   vr.Duration,
		HasVideo:   vr.Video > 0,
		Dst:        filepath.Join(dir, "folder.jpg"),
	})
	for _, a := range res.Attempts {
		rc.log.Printf("Thumbnail: %s", a)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		it.AddWarning("No thumbnail")
		rc.log.Line("WARNING: no thumbnail could be produced; the video is kept without folder.jpg")
	} else if res.Source == "frame" {
		rc.log.Line("Thumbnail unavailable, using a frame of the video")
	}

	if !rc.job.Options.Subtitles {
		return nil
	}
	want := meta.SubtitleLanguages()
	if len(want) == 0 {
		return nil
	}
	rc.log.Printf("Fetching subtitles: %s", strings.Join(sorted(want), ", "))
	sres := ytdlp.Run(ctx, ytdlp.Call{
		Bin: rc.bin.Path, Args: ytdlp.SubtitleArgs(dir, rc.settings.Extras.ConvertSubsSRT, rc.callOpts(p)),
		OnLine: func(l string) { rc.log.Line(l) },
	})
	if ctx.Err() != nil {
		return nil
	}
	p.noteImpersonation(rc, it, sres)
	subs := findSubs(dir)
	got := map[string]bool{}
	for _, s := range subs {
		got[s.Lang] = true
	}
	var missing []string
	for _, l := range want {
		if !got[l] {
			missing = append(missing, l)
		}
	}
	if !sres.OK() && len(subs) == 0 {
		it.AddWarning("Subtitles failed: " + ytdlp.Classify(sres).Reason)
	} else if len(missing) > 0 {
		sort.Strings(missing)
		it.AddWarning("Missing subtitles: " + strings.Join(missing, ", "))
	}
	if rc.settings.Extras.EmbedSubs && len(subs) > 0 {
		if err := p.embedSubs(ctx, rc, mediaPath, dir, subs); err != nil {
			it.AddWarning("Embedding subtitles failed")
			rc.log.Printf("WARNING: embedding subtitles failed: %v", err)
		}
	}
	return subs
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

var subExts = map[string]bool{"vtt": true, "srt": true, "ass": true, "ssa": true, "ttml": true, "srv1": true, "srv2": true, "srv3": true, "json3": true, "lrc": true, "dfxp": true, "sbv": true}

// findSubs lists media.<lang>.<ext> subtitle files in dir.
func findSubs(dir string) []subFile {
	entries, _ := os.ReadDir(dir)
	var out []subFile
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, ytdlp.MediaBase+".") {
			continue
		}
		rest := strings.TrimPrefix(name, ytdlp.MediaBase+".")
		i := strings.LastIndexByte(rest, '.')
		if i <= 0 {
			continue
		}
		lang, ext := rest[:i], rest[i+1:]
		if !subExts[ext] || lang == "live_chat" || strings.ContainsAny(lang, "/\x00") {
			continue
		}
		out = append(out, subFile{Name: name, Lang: lang, Ext: ext})
	}
	return out
}

// embedSubs muxes subtitles into the media file (setting, off by default).
// The result is verified before it replaces the original.
func (p *Pipeline) embedSubs(ctx context.Context, rc *runCtx, mediaPath, dir string, subs []subFile) error {
	ext := filepath.Ext(mediaPath)
	tmp := filepath.Join(dir, ".embed"+ext)
	defer os.Remove(tmp)
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", mediaPath}
	for _, s := range subs {
		args = append(args, "-i", filepath.Join(dir, s.Name))
	}
	args = append(args, "-map", "0")
	for i := range subs {
		args = append(args, "-map", strconv.Itoa(i+1))
	}
	subCodec := "srt"
	if ext == ".mp4" {
		subCodec = "mov_text"
	}
	args = append(args, "-c", "copy", "-c:s", subCodec)
	for i, s := range subs {
		args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "language="+s.Lang)
	}
	args = append(args, tmp)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.Command(p.d.Env.FFmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := procutil.Run(cctx, cmd, 5*time.Second); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	orig, err := p.verify(ctx, mediaPath, false, 0)
	if err != nil {
		return err
	}
	vr, err := p.verify(ctx, tmp, orig.Video > 0, orig.Audio)
	if err != nil {
		return err
	}
	if vr.Duration < orig.Duration*0.98 {
		return fmt.Errorf("muxed file is shorter than the original")
	}
	return os.Rename(tmp, mediaPath)
}

// toolVersions returns "ffmpeg X · ffprobe Y · deno Z" for the log header.
func toolVersions(env *config.Env) string {
	v := func(bin string, args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...)
		out, err := cmd.Output()
		if err != nil {
			return "unavailable"
		}
		line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		f := strings.Fields(line)
		for i, w := range f {
			if (w == "version" || w == "deno") && i+1 < len(f) {
				return f[i+1]
			}
		}
		return line
	}
	return fmt.Sprintf("ffmpeg %s · ffprobe %s · deno %s",
		v(env.FFmpegPath, "-version"), v(env.FFprobePath, "-version"), v(env.DenoPath, "--version"))
}

// ToolVersions is exported for the diagnostics page.
func ToolVersions(env *config.Env) map[string]string {
	parts := strings.Split(toolVersions(env), " · ")
	out := map[string]string{}
	for _, p := range parts {
		k, v, _ := strings.Cut(p, " ")
		out[k] = v
	}
	return out
}

// RerunExtras fetches the thumbnail and subtitles again for a finished
// item that is already in the library (§4.8). Files are written to a
// temporary name in the item folder, fsynced and renamed into place.
func (p *Pipeline) RerunExtras(ctx context.Context, jobID, itemID int64) error {
	job, err := p.d.Store.GetJob(bg, jobID)
	if err != nil {
		return err
	}
	it, err := p.d.Store.GetItem(bg, itemID)
	if err != nil || it.JobID != jobID {
		return store.ErrNotFound
	}
	if it.State != store.ItemDone || it.FinalPath == "" {
		return fmt.Errorf("item is not in the library")
	}
	if p.isRedditItem(it) {
		return fmt.Errorf("extras can't be re-run for Reddit posts; download the post again instead")
	}
	st := p.d.Settings()
	root, ok := st.LibraryPath(p.d.Env, it.Library)
	if !ok {
		return fmt.Errorf("library %q is not configured", it.Library)
	}
	folder := filepath.Join(root, filepath.FromSlash(it.FinalPath))
	if _, err := os.Stat(folder); err != nil {
		return fmt.Errorf("library folder is missing: %w", err)
	}
	rc := &runCtx{job: job, log: p.d.Logs.Get(jobID), settings: st, policy: p.policy(st)}
	defer p.d.Logs.Release(jobID)
	bin, release, err := p.d.Versions.Acquire(ctx, "")
	if err != nil {
		return err
	}
	defer release()
	rc.bin, rc.release = bin, func() {}
	rc.impersonate = job.Options.Impersonate
	if job.Retry.Impersonated && rc.impersonate == "" {
		rc.impersonate = "any"
	}
	dir := filepath.Join(JobWorkDir(p.d.Env, jobID), fmt.Sprintf("extras-%d", itemID))
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if job.Options.CookieProfile != "" {
		if path, err := p.prepareCookies(job); err == nil {
			rc.cookies = path
			defer os.Remove(path)
		}
	}
	rc.log.Printf("=== Re-running extras for %s ===", it.FinalPath)
	raw, meta, err := p.fetchMeta(ctx, rc, it, dir, it.URL, true)
	if err != nil {
		return fmt.Errorf("fetching metadata failed: %v", err)
	}
	_ = raw
	media := ""
	if len(it.Files) > 0 {
		media = filepath.Join(folder, it.Files[0].Dst)
	}
	it.Warnings = nil
	vr, verr := p.verify(ctx, media, false, 0)
	if verr != nil {
		vr = VerifyResult{}
	}
	subs := p.extras(ctx, rc, it, meta, dir, media, vr)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	base := strings.TrimSuffix(filepath.Base(media), filepath.Ext(media))
	install := func(src, dstName string) error {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		tmp := filepath.Join(folder, "."+dstName+".tmp")
		if err := writeSync(tmp, data); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, filepath.Join(folder, dstName)); err != nil {
			os.Remove(tmp)
			return err
		}
		return syncDir(folder)
	}
	files := it.Files[:0:0]
	files = append(files, it.Files...)
	have := map[string]bool{}
	for _, f := range files {
		have[f.Dst] = true
	}
	if _, err := os.Stat(filepath.Join(dir, "folder.jpg")); err == nil {
		if err := install(filepath.Join(dir, "folder.jpg"), "folder.jpg"); err != nil {
			it.AddWarning("Could not install thumbnail")
		} else if !have["folder.jpg"] {
			files = append(files, store.FileMove{Src: "folder.jpg", Dst: "folder.jpg"})
		}
	}
	for _, s := range subs {
		name := base + "." + s.Lang + "." + s.Ext
		if len(name) > 255 {
			continue
		}
		if err := install(filepath.Join(dir, s.Name), name); err != nil {
			it.AddWarning("Could not install subtitle " + s.Lang)
		} else if !have[name] {
			files = append(files, store.FileMove{Src: s.Name, Dst: name})
		}
	}
	it.Files = files
	p.saveItem(it)
	if len(it.Warnings) == 0 {
		rc.log.Line("✔ Extras updated")
	} else {
		rc.log.Printf("Extras updated with warnings: %s", strings.Join(it.Warnings, "; "))
	}
	p.d.Pub.JobChanged(job)
	return nil
}

func writeSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
