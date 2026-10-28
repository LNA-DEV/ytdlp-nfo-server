package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/failpoint"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// uid identifies an item's staging and trash entries.
func uid(it *store.Item) string { return fmt.Sprintf("%d-%d", it.JobID, it.ID) }

// UID is exported for crash recovery.
func UID(jobID, itemID int64) string { return fmt.Sprintf("%d-%d", jobID, itemID) }

func toPlanFiles(f []store.FileMove) []library.FileMove {
	out := make([]library.FileMove, len(f))
	for i, m := range f {
		out[i] = library.FileMove{Src: m.Src, Dst: m.Dst}
	}
	return out
}

// subFile is a subtitle in the work dir.
type subFile struct {
	Name string // media.<lang>.<ext>
	Lang string
	Ext  string
}

// finalize renders the library path, stores the plan, moves the item into
// the library and records the archive entry (§4.5).
func (p *Pipeline) finalize(ctx context.Context, rc *runCtx, it *store.Item, meta *ytdlp.Meta, dir, mediaPath string, subs []subFile) error {
	job := rc.job
	root, ok := rc.settings.LibraryPath(p.d.Env, job.Options.Library)
	if !ok {
		return failure(ytdlp.ClassPermanent, fmt.Sprintf("library %q is not configured", job.Options.Library))
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return failure(ytdlp.ClassTransient, "cannot create library root: "+err.Error())
	}
	tpl, err := library.ParseTemplate(rc.settings.Naming.EffectiveTemplate())
	if err != nil {
		return failure(ytdlp.ClassPermanent, "naming template: "+err.Error())
	}
	ext := strings.TrimPrefix(filepath.Ext(mediaPath), ".")
	year := ""
	if len(meta.UploadDate) >= 4 {
		year = meta.UploadDate[:4]
	}
	fields := map[string]string{
		"title": firstNonEmpty(meta.Title, it.Title), "id": meta.ID, "uploader": meta.Uploader,
		"channel": meta.Channel, "upload_date": meta.UploadDate, "upload_year": year,
		"playlist_title": it.PlaylistTitle, "extractor": meta.Extractor, "ext": ext,
	}
	rendered := tpl.Render(fields)

	unlockRoot := library.LockRoot(root)
	defer unlockRoot()
	target, replace, err := library.ChooseTarget(root, rendered, meta.ID)
	if err != nil {
		return failure(ytdlp.ClassTransient, err.Error())
	}
	if replace {
		rc.log.Printf("Replacing existing library folder of the same video: %s", target.Dir())
	}

	moves := []store.FileMove{{Src: filepath.Base(mediaPath), Dst: target.FileName(), Size: it.Size}}
	for _, s := range subs {
		dst := target.Base + "." + s.Lang + "." + s.Ext
		if len(dst) > 255 {
			it.AddWarning("Subtitle " + s.Lang + " skipped (file name too long)")
			continue
		}
		moves = append(moves, store.FileMove{Src: s.Name, Dst: dst})
	}
	if _, err := os.Stat(filepath.Join(dir, "folder.jpg")); err == nil {
		moves = append(moves, store.FileMove{Src: "folder.jpg", Dst: "folder.jpg"})
	}
	moves = append(moves, store.FileMove{Src: "movie.nfo", Dst: "movie.nfo"})

	// Record the plan before touching the library, so a crash at any point
	// can be resumed (step 1).
	it.State = store.ItemFinalizing
	it.Library = job.Options.Library
	it.FinalPath = target.Dir()
	it.Files = moves
	p.saveItem(it)
	p.publishStage(rc, it, "finalizing")
	return p.runFinalize(rc, it, root, dir, meta.ID)
}

// runFinalize executes the stored plan; it is not cancellable.
func (p *Pipeline) runFinalize(rc *runCtx, it *store.Item, root, dir, videoID string) error {
	plan := library.Plan{UID: uid(it), Root: root, Target: it.FinalPath, WorkDir: dir, Files: toPlanFiles(it.Files), VideoID: videoID}
	err := p.d.Finalizer.Finalize(plan)
	for tries := 0; errors.Is(err, library.ErrTargetTaken) && tries < 5; tries++ {
		// Something else created the folder meanwhile: pick a free one. The
		// staged files keep their names; only the folder changes.
		cur := library.Rendered{Dirs: strings.Split(it.FinalPath, "/"), Base: "_", Ext: "_"}
		target, _, cerr := library.ChooseTarget(root, cur, videoID)
		if cerr != nil {
			err = cerr
			break
		}
		it.FinalPath = target.Dir()
		plan.Target = it.FinalPath
		p.saveItem(it)
		rc.log.Printf("Target folder is taken by another video, using %s", it.FinalPath)
		err = p.d.Finalizer.Finalize(plan)
	}
	if err != nil {
		rc.log.Printf("Finalize failed: %v", err)
		return failure(ytdlp.ClassTransient, "moving into the library failed: "+err.Error())
	}

	it.State = store.ItemDone
	it.FinishedAt = p.d.Now()
	it.Error, it.ErrorClass, it.Hint = "", "", ""
	it.RetryAt = timeZero
	entry := store.ArchiveEntry{
		Key: it.ArchiveKey, Extractor: strings.ToLower(it.Extractor), VideoID: it.VideoID,
		Path: it.FinalPath, JobID: it.JobID, FinalizedAt: it.FinishedAt, Source: "server",
	}
	if err := p.d.Store.FinalizeItem(bg, it, entry, mediaRows(it, root)...); err != nil {
		// The files are in place; the record will be repaired by a retry
		// (finalize is idempotent and will find the target complete).
		it.State = store.ItemFinalizing
		return failure(ytdlp.ClassTransient, "recording the archive entry failed: "+err.Error())
	}
	p.d.Pub.ItemChanged(it)
	_ = failpoint.Check("pipeline:recorded")
	if it.ArchiveKey != "" && library.FileKey(it.ArchiveKey) {
		if err := p.d.Archive.Append(it.ArchiveKey); err != nil {
			rc.log.Printf("WARNING: could not append to %s: %v", p.d.Archive.Path, err)
		}
	}
	if err := p.d.Finalizer.Cleanup(plan); err != nil {
		rc.log.Printf("WARNING: cleanup after finalize: %v", err)
	}
	rc.log.Printf("✔ Finalized: %s", it.FinalPath)
	for _, w := range it.Warnings {
		rc.log.Printf("WARNING: %s", w)
	}
	p.d.Pub.ItemFinalized(rc.job, it, root)
	return nil
}

// resumeFinalize continues an interrupted finalize from its stored plan.
func (p *Pipeline) resumeFinalize(ctx context.Context, rc *runCtx, it *store.Item) itemResult {
	root, ok := rc.settings.LibraryPath(p.d.Env, it.Library)
	if !ok {
		root, _ = rc.settings.LibraryPath(p.d.Env, rc.job.Options.Library)
	}
	rc.log.Printf("Resuming finalize of %s", it.FinalPath)
	dir := ItemWorkDir(p.d.Env, rc.job.ID, it.ID)
	unlockRoot := library.LockRoot(root)
	err := p.runFinalize(rc, it, root, dir, it.VideoID)
	unlockRoot()
	if err == nil {
		return resultDone
	}
	var af *attemptFailure
	if errors.As(err, &af) {
		p.itemFailure(ctx, rc, it, af.cls)
	}
	return resultDone
}
