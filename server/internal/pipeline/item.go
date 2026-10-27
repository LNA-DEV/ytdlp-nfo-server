package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/failpoint"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/formats"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/nfo"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

type itemResult int

const (
	resultDone    itemResult = iota // terminal or waiting for a retry
	resultStop                      // cancelled / shutdown
	resultRequeue                   // low disk space: give the slot back
)

// attemptFailure is a classified failure of one attempt.
type attemptFailure struct {
	cls ytdlp.Classification
}

func (e *attemptFailure) Error() string { return e.cls.Reason }

func failure(class ytdlp.Class, reason string) error {
	return &attemptFailure{cls: ytdlp.Classification{Class: class, Reason: reason, Norm: ytdlp.Normalize(reason)}}
}

var errStop = errors.New("stopped")
var errRequeue = errors.New("requeue")
var errSkipped = errors.New("skipped")

func (p *Pipeline) processItem(ctx context.Context, rc *runCtx, it *store.Item) itemResult {
	rc.log.Printf("=== Item %d/%d: %s ===", it.Index+1, rc.itemCount, displayTitle(it))
	it.RetryAt = time.Time{}

	// A crash or failure during finalize left a staged copy: resume it.
	if it.State == store.ItemFinalizing && it.FinalPath != "" && len(it.Files) > 0 {
		return p.resumeFinalize(ctx, rc, it)
	}

	unlock, err := p.keys.lock(ctx, it.ArchiveKey, func() {
		rc.log.Line("Waiting: the same video is being processed by another job")
	})
	if err != nil {
		return resultStop
	}
	defer func() { unlock() }()
	if p.skipIfArchived(rc, it) {
		return resultDone
	}

	refetch := false
	for {
		err := p.attempt(ctx, rc, it, refetch, &unlock)
		refetch = false
		switch {
		case err == nil:
			return resultDone
		case errors.Is(err, errSkipped):
			return resultDone
		case ctx.Err() != nil || errors.Is(err, errStop):
			return resultStop
		case errors.Is(err, errRequeue):
			return resultRequeue
		}
		var af *attemptFailure
		if !errors.As(err, &af) {
			af = &attemptFailure{cls: ytdlp.Classification{Class: ytdlp.ClassUnknown, Reason: err.Error(), Norm: ytdlp.Normalize(err.Error())}}
		}
		retryNow, escalation := p.itemFailure(ctx, rc, it, af.cls)
		if !retryNow {
			return resultDone
		}
		if escalation == scheduler.EscalateRefetch {
			refetch = true
		}
	}
}

func displayTitle(it *store.Item) string {
	if it.Title != "" {
		return it.Title
	}
	return it.URL
}

func (p *Pipeline) skipIfArchived(rc *runCtx, it *store.Item) bool {
	if it.ArchiveKey == "" || rc.job.Options.Force || !p.inArchive(it.ArchiveKey) {
		return false
	}
	it.State = store.ItemSkipped
	it.Error, it.ErrorClass, it.Hint = "Already in library", "", ""
	it.FinishedAt = p.d.Now()
	p.saveItem(it)
	// Subscription entries listed without a video id follow the item to
	// its archive key, so they show as in library.
	if err := p.d.Store.RekeyEntries(bg, it.ID, it.ArchiveKey); err != nil {
		rc.log.Printf("WARNING: updating subscription entries: %v", err)
	}
	rc.log.Printf("Already in library: %s", displayTitle(it))
	os.RemoveAll(ItemWorkDir(p.d.Env, rc.job.ID, it.ID))
	return true
}

// itemFailure applies the retry policy. It returns whether to retry now.
func (p *Pipeline) itemFailure(ctx context.Context, rc *runCtx, it *store.Item, cls ytdlp.Classification) (bool, string) {
	it.Attempts++
	sit := scheduler.Situation{
		Class: cls, Failures: it.Attempts,
		ImpersonationAvailable: rc.bin.ImpersonateAvailable, Impersonating: rc.impersonate != "",
		NightlyCanHelp: p.nightlyCanHelp(ctx, rc),
	}
	d := rc.policy.Decide(&it.Retry, sit)
	at := p.d.Now().Add(d.Delay)
	rc.log.Line(scheduler.LogLine(d, sit, budget(rc.policy, cls), at))
	if cls.Hint == "" {
		cls.Hint = defaultHint(cls)
	}
	it.Error, it.ErrorClass = cls.Reason, string(cls.Class)
	if !d.Retry {
		if strings.Contains(d.Why, "impersonation is unavailable") {
			cls.Hint = ytdlp.HintBlocked + " Impersonation is not available in this yt-dlp build."
		}
		if it.State != store.ItemFinalizing {
			it.State = store.ItemFailed
		} else {
			// Keep the staged copy: a manual retry resumes finalize.
			it.State = store.ItemFailed
			cls.Hint = "Finalizing failed; the downloaded files are kept in staging. Retry to finish the move. " + cls.Hint
		}
		it.Hint = cls.Hint
		it.FinishedAt = p.d.Now()
		p.saveItem(it)
		return false, ""
	}
	if d.Escalate != "" {
		if d.Escalate == scheduler.EscalateImpersonate {
			rc.job.Retry.Impersonated = true
		}
		if d.Escalate == scheduler.EscalateNightly {
			rc.job.Retry.NightlyTried = true
		}
		if !p.escalate(ctx, rc, d.Escalate) {
			it.State = store.ItemFailed
			it.Hint = cls.Hint
			it.FinishedAt = p.d.Now()
			p.saveItem(it)
			return false, ""
		}
		p.saveJob(rc.job)
		if it.State != store.ItemFinalizing {
			it.State = store.ItemPending
		}
		p.saveItem(it)
		return true, d.Escalate
	}
	if d.Cooldown > 0 {
		p.d.Sched.SetCooldown(rc.job.Site, p.d.Now().Add(d.Cooldown))
		cls.Hint = fmt.Sprintf("Rate-limited by the site, resuming at %s", at.Format("15:04"))
	}
	it.Hint = cls.Hint
	if it.State != store.ItemFinalizing {
		it.State = store.ItemPending
	}
	if d.Delay > 0 {
		it.RetryAt = at
		p.saveItem(it)
		return false, ""
	}
	p.saveItem(it)
	return true, ""
}

// attempt runs one attempt of an item up to and including finalize.
func (p *Pipeline) attempt(ctx context.Context, rc *runCtx, it *store.Item, refetch bool, unlock *func()) error {
	job := rc.job
	dir := ItemWorkDir(p.d.Env, job.ID, it.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return failure(ytdlp.ClassTransient, "cannot create work dir: "+err.Error())
	}

	// --- fetch meta (just in time) ---
	it.State = store.ItemFetchingMeta
	it.Error, it.ErrorClass, it.Hint = "", "", ""
	p.saveItem(it)
	p.publishStage(rc, it, "fetching metadata")
	raw, meta, err := p.fetchMeta(ctx, rc, it, dir, refetch)
	if err != nil {
		return err
	}
	if meta.IsPlaylist {
		if job.Planned() {
			// A subscription item must stay one video: expanding it could
			// bypass the auto-queue cap (subscriptions concept §4.3).
			return failure(ytdlp.ClassPermanent, "not a single video: the URL is a playlist or channel")
		}
		return p.expandItem(ctx, rc, it, raw)
	}
	keyBefore := it.ArchiveKey
	it.Extractor = firstNonEmpty(meta.ExtractorKey, it.Extractor)
	it.VideoID = firstNonEmpty(meta.ID, it.VideoID)
	if k := ytdlp.ArchiveKey(it.Extractor, it.VideoID); k != "" {
		it.ArchiveKey = k
	}
	it.Title = firstNonEmpty(meta.Title, it.Title)
	it.Duration = meta.Duration
	if job.Thumbnail == "" && meta.Thumbnail != "" && !job.IsPlaylist {
		job.Thumbnail = meta.Thumbnail
		p.saveJob(job)
	}
	if it.ArchiveKey != keyBefore {
		(*unlock)()
		u, err := p.keys.lock(ctx, it.ArchiveKey, func() { rc.log.Line("Waiting: the same video is being processed by another job") })
		if err != nil {
			return errStop
		}
		*unlock = u
		if p.skipIfArchived(rc, it) {
			return errSkipped
		}
	}

	// --- select formats ---
	sortStr := rc.settings.Presets[job.Options.Preset]
	sel := formats.Select(meta.Formats, job.Options.AllAudio, sortStr)
	if len(sel.Tracks) > 1 {
		labels := make([]string, len(sel.Tracks))
		for i, t := range sel.Tracks {
			labels[i] = t.Label()
		}
		rc.log.Printf("Found %d audio tracks: %s", len(sel.Tracks), strings.Join(labels, ", "))
	}

	// --- disk space ---
	if err := p.checkDisk(rc, meta.FilesizeHint); err != nil {
		rc.log.Printf("--- %v. Pausing the queue ---", err)
		p.d.Sched.Pause(err.Error(), p.d.Now().Add(5*time.Minute))
		p.d.Pub.SystemWarning("disk", err.Error())
		it.State = store.ItemPending
		p.saveItem(it)
		return errRequeue
	}

	// --- download (media only) ---
	it.State = store.ItemDownloading
	it.YtdlpVersion = rc.bin.ID
	p.saveItem(it)
	mediaPath, formatID, err := p.download(ctx, rc, it, dir, sel, formats.AudioOnlySource(meta.Formats))
	if err != nil {
		return err
	}
	_ = failpoint.Check("pipeline:downloaded")

	// --- verify ---
	it.State = store.ItemVerifying
	p.saveItem(it)
	p.publishStage(rc, it, "verifying")
	comp := formats.Describe(meta.Formats, formatID)
	expectVideo := !formats.AudioOnlySource(meta.Formats) && (!comp.Known || comp.Video > 0)
	expectAudio := 0
	if comp.Known {
		expectAudio = comp.Audio
	}
	vr, err := p.verify(ctx, mediaPath, expectVideo, expectAudio)
	if err != nil {
		if ctx.Err() != nil {
			return errStop
		}
		os.Remove(mediaPath)
		return failure(ytdlp.ClassTransient, "verification failed: "+err.Error())
	}
	rc.log.Printf("Verified %s: %.0fs, %d video and %d audio stream(s), %s",
		filepath.Base(mediaPath), vr.Duration, vr.Video, vr.Audio, humanBytes(vr.Size))
	it.Size = vr.Size
	if it.Duration == 0 {
		it.Duration = vr.Duration
	}
	_ = failpoint.Check("pipeline:verified")

	// --- extras (best-effort) ---
	it.State = store.ItemExtras
	p.saveItem(it)
	p.publishStage(rc, it, "thumbnail and subtitles")
	subs := p.extras(ctx, rc, it, meta, dir, mediaPath, vr)
	if ctx.Err() != nil {
		return errStop
	}

	// --- NFO (required) ---
	info, err := nfo.ParseInfo(raw)
	if err != nil {
		return failure(ytdlp.ClassUnknown, "cannot read info.json for the NFO: "+err.Error())
	}
	rules := make([]nfo.AgeRule, len(rc.settings.AgeRules))
	for i, r := range rc.settings.AgeRules {
		rules[i] = nfo.AgeRule{Match: r.Match, Rating: r.Rating}
	}
	nfoData := nfo.Movie(info, nfo.Options{AgeRules: rules, DateAdded: p.d.Now()})
	if err := os.WriteFile(filepath.Join(dir, "movie.nfo"), nfoData, 0o644); err != nil {
		return failure(ytdlp.ClassTransient, "writing movie.nfo failed: "+err.Error())
	}
	_ = failpoint.Check("pipeline:extras-done")

	// --- finalize ---
	return p.finalize(ctx, rc, it, meta, dir, mediaPath, subs)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// fetchMeta returns info.json, extracting it just in time unless a fresh
// copy exists (the single-video probe result).
func (p *Pipeline) fetchMeta(ctx context.Context, rc *runCtx, it *store.Item, dir string, refetch bool) ([]byte, *ytdlp.Meta, error) {
	path := filepath.Join(dir, ytdlp.InfoFile)
	if fi, err := os.Stat(path); err == nil && !refetch && p.d.Now().Sub(fi.ModTime()) < p.d.MetaMaxAge {
		if raw, err := os.ReadFile(path); err == nil {
			if meta, err := ytdlp.ParseMeta(raw); err == nil {
				return raw, meta, nil
			}
		}
	}
	if config.ValidateURL(it.URL) != nil {
		return nil, nil, failure(ytdlp.ClassPermanent, "entry has no usable URL")
	}
	rc.log.Printf("Fetching metadata for %s", it.URL)
	res := ytdlp.Run(ctx, ytdlp.Call{Bin: rc.bin.Path, Args: ytdlp.MetaArgs(it.URL, rc.callOpts(p)), CaptureStdout: true,
		OnLine: func(l string) { rc.log.Line(l) }})
	if ctx.Err() != nil {
		return nil, nil, errStop
	}
	p.noteImpersonation(rc, it, res)
	if !res.OK() {
		return nil, nil, &attemptFailure{cls: ytdlp.Classify(res)}
	}
	meta, err := ytdlp.ParseMeta(res.Stdout)
	if err != nil {
		return nil, nil, failure(ytdlp.ClassUnknown, err.Error())
	}
	if err := library.WriteFileAtomic(path, res.Stdout, 0o644); err != nil {
		return nil, nil, failure(ytdlp.ClassTransient, "writing info.json failed: "+err.Error())
	}
	return res.Stdout, meta, nil
}

// expandItem replaces an entry that turned out to be a playlist by its
// videos, inserted right after it.
func (p *Pipeline) expandItem(ctx context.Context, rc *runCtx, it *store.Item, raw []byte) error {
	pr, err := ytdlp.ParseProbe(raw)
	if err != nil {
		return failure(ytdlp.ClassUnknown, err.Error())
	}
	entries, err := p.expandNested(ctx, rc, pr.Entries, 1)
	if err != nil {
		return errStop
	}
	if err := p.d.Store.ShiftItemIndexes(bg, rc.job.ID, it.Index, len(entries)); err != nil {
		return failure(ytdlp.ClassTransient, err.Error())
	}
	pl := it.PlaylistTitle
	if pl == "" {
		pl = pr.Title
	}
	items, skipped := p.newItems(rc, entries, it.Index+1, pl)
	if err := p.d.Store.CreateItems(bg, items); err != nil {
		return failure(ytdlp.ClassTransient, err.Error())
	}
	rc.log.Printf("Entry is a playlist: added %d items (%d already in library)", len(items), skipped)
	it.State = store.ItemSkipped
	it.Error = fmt.Sprintf("Playlist expanded into %d items", len(items))
	it.FinishedAt = p.d.Now()
	p.saveItem(it)
	os.RemoveAll(ItemWorkDir(p.d.Env, rc.job.ID, it.ID))
	return errSkipped
}

func (p *Pipeline) checkDisk(rc *runCtx, estimate int64) error {
	minFree := uint64(max(rc.settings.MinFreeSpaceMB, 0)) << 20
	work := p.d.Env.WorkRoot()
	need := max(uint64(float64(max(estimate, 0))*2.2), minFree)
	if free, err := library.FreeBytes(work); err == nil && free < need {
		return fmt.Errorf("low disk space in the download directory: %s free, %s needed", humanBytes(int64(free)), humanBytes(int64(need)))
	}
	root, ok := rc.settings.LibraryPath(p.d.Env, rc.job.Options.Library)
	if !ok {
		return nil
	}
	_ = os.MkdirAll(root, 0o755)
	if !library.SameFilesystem(work, root) {
		need := max(uint64(max(estimate, 0)), minFree)
		if free, err := library.FreeBytes(root); err == nil && free < need {
			return fmt.Errorf("low disk space in the library: %s free, %s needed", humanBytes(int64(free)), humanBytes(int64(need)))
		}
	}
	return nil
}

// download runs the media-only download and returns the media path and the
// chosen format id.
func (p *Pipeline) download(ctx context.Context, rc *runCtx, it *store.Item, dir string, sel formats.Selection, audioOnly bool) (string, string, error) {
	resultPath := filepath.Join(dir, ytdlp.ResultFile)
	os.Remove(resultPath)
	spec := ytdlp.DownloadSpec{
		WorkDir: dir, Format: sel.Format, AudioMultistream: sel.AudioMultistream, Sort: sel.Sort,
		Container: rc.job.Options.Container, AudioOnly: audioOnly, EmbedChapters: rc.settings.Extras.EmbedChapters,
		SponsorBlockMark: rc.settings.Extras.SponsorBlockMark,
	}
	parts := strings.Count(strings.SplitN(sel.Format, "/", 2)[0], "+") + 1
	files := map[string]int{}
	res := ytdlp.Run(ctx, ytdlp.Call{
		Bin: rc.bin.Path, Args: ytdlp.DownloadArgs(spec, rc.callOpts(p)),
		OnLine: func(l string) { rc.log.Line(l) },
		OnProgress: func(pr ytdlp.Progress) {
			_ = failpoint.Check("pipeline:downloading")
			if _, ok := files[pr.Filename]; !ok {
				files[pr.Filename] = len(files) + 1
			}
			p.publishProgress(rc, it, pr, files[pr.Filename], max(parts, len(files)))
		},
	})
	if ctx.Err() != nil {
		return "", "", errStop
	}
	p.noteImpersonation(rc, it, res)
	if !res.OK() {
		return "", "", &attemptFailure{cls: ytdlp.Classify(res)}
	}
	media, formatID := readResult(resultPath, dir)
	if media == "" {
		media = findMedia(dir)
	}
	if media == "" {
		return "", "", failure(ytdlp.ClassUnknown, "yt-dlp reported success but produced no media file")
	}
	return media, formatID, nil
}

// readResult parses the --print-to-file output ("<path>\t<format_id>"),
// accepting only a regular file inside the work dir.
func readResult(path, dir string) (string, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	last := lines[len(lines)-1]
	file, formatID, _ := strings.Cut(last, "\t")
	file = filepath.Clean(strings.TrimSpace(file))
	if !filepath.IsAbs(file) {
		file = filepath.Join(dir, file)
	}
	if filepath.Dir(file) != filepath.Clean(dir) {
		return "", ""
	}
	if fi, err := os.Stat(file); err != nil || !fi.Mode().IsRegular() {
		return "", ""
	}
	return file, strings.TrimSpace(formatID)
}

// findMedia looks for media.<ext> when no result file was written.
func findMedia(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, ytdlp.MediaBase+".*"))
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.Count(base, ".") != 1 || strings.HasSuffix(base, ".part") || strings.HasSuffix(base, ".ytdl") {
			continue
		}
		if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			return m
		}
	}
	return ""
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// --- progress ---

func (p *Pipeline) publishStage(rc *runCtx, it *store.Item, stage string) {
	p.d.Pub.Progress(Live{JobID: rc.job.ID, ItemID: it.ID, ItemIndex: it.Index, ItemCount: rc.itemCount,
		ItemTitle: displayTitle(it), Stage: stage})
	rc.lastPublish = time.Time{}
}

// publishProgress throttles progress to at most 2 updates per second.
func (p *Pipeline) publishProgress(rc *runCtx, it *store.Item, pr ytdlp.Progress, part, parts int) {
	now := time.Now()
	if pr.Status != "finished" && now.Sub(rc.lastPublish) < 500*time.Millisecond {
		return
	}
	rc.lastPublish = now
	total := pr.TotalBytes
	if total <= 0 {
		total = pr.TotalEstimate
	}
	p.d.Pub.Progress(Live{
		JobID: rc.job.ID, ItemID: it.ID, ItemIndex: it.Index, ItemCount: rc.itemCount, ItemTitle: displayTitle(it),
		Stage: "downloading", Percent: pr.Percent(), Speed: pr.Speed, ETA: pr.ETA,
		Downloaded: pr.DownloadedBytes, Total: total, Part: part, Parts: parts,
	})
}
