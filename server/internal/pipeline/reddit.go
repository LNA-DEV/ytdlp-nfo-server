package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/artwork"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/failpoint"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/nfo"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// RedditAccess gives Reddit items their posts and media (Reddit concept
// §7.2): the provider chain and the browser client for media downloads.
type RedditAccess interface {
	Chain(providers string, logf func(format string, args ...any)) *reddit.Chain
	Client() *browser.Client
}

// PostFile is the post as fetched for a Reddit item; it replaces
// info.json for images and is subject to the same freshness rule.
const PostFile = "post.json"

// Size limits of Reddit media (Reddit concept §7.3).
const (
	maxImageBytes   = 50 << 20
	maxGalleryBytes = 2 << 30
	maxThumbBytes   = 20 << 20
)

// isRedditItem reports whether attemptReddit handles an item: its URL is
// a Reddit post. The key can change once (post key to media key), the URL
// doesn't.
func (p *Pipeline) isRedditItem(it *store.Item) bool {
	return p.d.Reddit != nil && reddit.IsPostURL(it.URL)
}

func (p *Pipeline) redditChain(rc *runCtx) *reddit.Chain {
	return p.d.Reddit.Chain(rc.job.Options.RedditProviders, func(f string, a ...any) { rc.log.Printf("Reddit "+f, a...) })
}

func redditFailure(e *reddit.Error) error {
	return &attemptFailure{cls: ytdlp.Classification{Class: e.Class, Reason: e.Reason, Hint: e.Hint, Norm: ytdlp.Normalize(e.Reason)}}
}

// writesArchiveFile reports whether a key goes to .ytdlp-archive.txt:
// yt-dlp-compatible tools know Reddit video keys, not image or gallery
// keys (Reddit concept §7.2).
func writesArchiveFile(key string) bool {
	return !strings.HasPrefix(key, "redditmedia ") && !strings.HasPrefix(key, "redditgallery ") &&
		!strings.HasPrefix(key, "redditpost ")
}

// attemptReddit runs one attempt of a Reddit item: fetch the post, re-key
// it to its media key, download and verify its images or video, and
// finalize it with a marker (Reddit concept §7.2).
func (p *Pipeline) attemptReddit(ctx context.Context, rc *runCtx, it *store.Item, refetch bool, unlock *func()) error {
	job := rc.job
	dir := ItemWorkDir(p.d.Env, job.ID, it.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return failure(ytdlp.ClassTransient, "cannot create work dir: "+err.Error())
	}
	it.State = store.ItemFetchingMeta
	it.Error, it.ErrorClass, it.Hint = "", "", ""
	p.saveItem(it)
	p.publishStage(rc, it, "fetching post")
	post, err := p.fetchPost(ctx, rc, it, dir, refetch)
	if err != nil {
		return err
	}
	it.Title = firstNonEmpty(post.Title, it.Title)
	if job.Thumbnail == "" && post.Thumbnail != "" && !job.IsPlaylist {
		job.Thumbnail = post.Thumbnail
		p.saveJob(job)
	}
	switch post.Kind {
	case reddit.KindText:
		return failure(ytdlp.ClassPermanent, "a text post without media")
	case reddit.KindLink:
		return failure(ytdlp.ClassPermanent, "a link to "+post.LinkHost()+"; links to other sites are not downloaded yet")
	case reddit.KindImage, reddit.KindGIF, reddit.KindGallery, reddit.KindVideo:
	default:
		return failure(ytdlp.ClassUnknown, "the post's media could not be read")
	}
	if post.Removed && post.Kind != reddit.KindGallery {
		return failure(ytdlp.ClassPermanent, "the post's media was removed from Reddit")
	}
	ext, id := post.MediaKeyParts()
	if id == "" {
		return failure(ytdlp.ClassUnknown, "the post's media could not be read")
	}
	keyBefore := it.ArchiveKey
	it.Extractor, it.VideoID, it.ArchiveKey = ext, id, strings.ToLower(ext)+" "+id
	if it.ArchiveKey != keyBefore {
		(*unlock)()
		u, err := p.keys.lock(ctx, it.ArchiveKey, func() { rc.log.Line("Waiting: the same media is being processed by another job") })
		if err != nil {
			return errStop
		}
		*unlock = u
		if p.skipIfArchived(rc, it) {
			return errSkipped
		}
	}
	p.saveItem(it)
	if post.Kind == reddit.KindVideo {
		return p.redditVideo(ctx, rc, it, post, dir)
	}
	return p.redditImages(ctx, rc, it, post, dir)
}

// fetchPost returns the post of an item, from a fresh post.json or
// through the provider chain. A crosspost gets the media of its original.
func (p *Pipeline) fetchPost(ctx context.Context, rc *runCtx, it *store.Item, dir string, refetch bool) (*reddit.Post, error) {
	path := filepath.Join(dir, PostFile)
	if fi, err := os.Stat(path); err == nil && !refetch && p.d.Now().Sub(fi.ModTime()) < p.d.MetaMaxAge {
		if raw, err := os.ReadFile(path); err == nil {
			var post reddit.Post
			if json.Unmarshal(raw, &post) == nil && post.ID != "" {
				return &post, nil
			}
		}
	}
	t, ok := reddit.Parse(it.URL)
	if !ok || t.Kind != reddit.TargetPost {
		return nil, failure(ytdlp.ClassPermanent, "not a Reddit post URL")
	}
	rc.log.Printf("Fetching post %s", it.URL)
	chain := p.redditChain(rc)
	post, provider, rerr := chain.Post(ctx, t.PostID, t.Subreddit)
	if ctx.Err() != nil {
		return nil, errStop
	}
	if rerr != nil {
		return nil, redditFailure(rerr)
	}
	if post.Provider == "" {
		post.Provider = provider
	}
	if post.Kind == "" && post.CrosspostParent != "" {
		orig, _, rerr := chain.Post(ctx, post.CrosspostParent, "")
		if ctx.Err() != nil {
			return nil, errStop
		}
		if rerr != nil {
			return nil, redditFailure(rerr)
		}
		rc.log.Printf("Crosspost of r/%s: %s", orig.Subreddit, orig.Title)
		post.Kind, post.Media, post.VideoID, post.Link, post.Removed = orig.Kind, orig.Media, orig.VideoID, orig.Link, orig.Removed
		post.Duration = orig.Duration
		if post.Thumbnail == "" {
			post.Thumbnail = orig.Thumbnail
		}
	}
	raw, err := json.MarshalIndent(post, "", "  ")
	if err != nil {
		return nil, failure(ytdlp.ClassUnknown, err.Error())
	}
	if err := library.WriteFileAtomic(path, raw, 0o644); err != nil {
		return nil, failure(ytdlp.ClassTransient, "writing post.json failed: "+err.Error())
	}
	return post, nil
}

// redditFile is a verified media file in the work dir.
type redditFile struct {
	pos   int    // position in the gallery (1-based), 0 for a single file
	name  string // file name in the work dir
	ext   string
	media string // media key
	sha   string
	size  int64
}

// knownImage is an image of a gallery that is already in the library.
type knownImage struct {
	library.MarkerKnown
	archiveKey string
}

// redditImages downloads an image, a GIF or a gallery (Reddit concept
// §7.2 steps 3 and 4) with the duplicate levels 1 and 2 (§7.5).
func (p *Pipeline) redditImages(ctx context.Context, rc *runCtx, it *store.Item, post *reddit.Post, dir string) error {
	force := rc.job.Options.Force
	gallery := post.Kind == reddit.KindGallery
	if len(post.Media) == 0 {
		return failure(ytdlp.ClassUnknown, "the post lists no images")
	}
	if err := p.checkDisk(rc, 0); err != nil {
		rc.log.Printf("--- %v. Pausing the queue ---", err)
		p.d.Sched.Pause(err.Error(), p.d.Now().Add(5*time.Minute))
		p.d.Pub.SystemWarning("disk", err.Error())
		it.State = store.ItemPending
		p.saveItem(it)
		return errRequeue
	}
	it.State = store.ItemDownloading
	p.saveItem(it)
	var (
		files   []redditFile
		known   []knownImage
		removed int
		total   int64
	)
	n := len(post.Media)
	for i, m := range post.Media {
		pos, base := 0, "media"
		if gallery {
			pos, base = i+1, fmt.Sprintf("media-%02d", i+1)
		}
		mediaKey := reddit.ImageMediaKey(m.MediaID)
		// Level 1: an image of the gallery that is already somewhere in the
		// library is not stored again (decision 6).
		if gallery && !force {
			if row, err := p.d.Store.MediaByKey(bg, mediaKey); err == nil && row != nil {
				known = append(known, knownImage{library.MarkerKnown{Position: pos, Media: mediaKey, Path: row.Path}, row.ArchiveKey})
				continue
			}
		}
		if n > 1 {
			p.publishStage(rc, it, fmt.Sprintf("downloading image %d of %d", i+1, n))
		} else {
			p.publishStage(rc, it, "downloading")
		}
		f, gone, err := p.redditImage(ctx, rc, dir, base, m.URL)
		if err != nil {
			return err
		}
		if gone {
			removed++
			rc.log.Printf("Image %d of %d was removed from Reddit", i+1, n)
			continue
		}
		f.pos, f.media = pos, mediaKey
		if total += f.size; total > maxGalleryBytes {
			return failure(ytdlp.ClassPermanent, fmt.Sprintf("the gallery is larger than %d GB", maxGalleryBytes>>30))
		}
		// Level 2: an identical file is already in the library.
		if !force {
			if row, err := p.d.Store.MediaBySHA(bg, f.sha); err == nil && row != nil {
				if !gallery {
					return p.skipDuplicate(rc, it, "Identical to "+row.Path,
						[]store.MediaRow{{MediaKey: mediaKey, SHA256: f.sha, Alias: true}})
				}
				rc.log.Printf("Image %d of %d is identical to %s", i+1, n, row.Path)
				known = append(known, knownImage{library.MarkerKnown{Position: pos, Media: mediaKey, Path: row.Path, SHA256: f.sha}, row.ArchiveKey})
				os.Remove(filepath.Join(dir, f.name))
				continue
			}
		}
		files = append(files, f)
	}
	switch {
	case removed == n && n == 1:
		return failure(ytdlp.ClassPermanent, "the image was removed from Reddit")
	case removed == n:
		return failure(ytdlp.ClassPermanent, "all images of the gallery were removed from Reddit")
	case removed > 0:
		it.AddWarning(fmt.Sprintf("%d of %d images were removed", removed, n))
	}
	if len(files) == 0 {
		// Everything is in the library already: the item is a duplicate.
		// The gallery key points at the first known image, so its entries
		// show as in library.
		aliases := []store.MediaRow{{MediaKey: it.ArchiveKey, ArchiveKey: known[0].archiveKey, Path: known[0].Path, Alias: true}}
		for _, k := range known {
			if k.SHA256 != "" {
				aliases = append(aliases, store.MediaRow{MediaKey: k.Media, SHA256: k.SHA256, Alias: true})
			}
		}
		return p.skipDuplicate(rc, it, fmt.Sprintf("All %d images are already in the library", len(known)), aliases)
	}
	if len(known) > 0 {
		paths := make([]string, len(known))
		for i, k := range known {
			paths[i] = k.Path
		}
		it.AddWarning(fmt.Sprintf("%d of %d images were already in the library and were not stored again: %s",
			len(known), n, strings.Join(paths, ", ")))
	}
	it.Size = total
	for _, f := range files {
		setPostTime(filepath.Join(dir, f.name), post.Created)
	}
	marked := make([]library.MarkerKnown, len(known))
	for i, k := range known {
		marked[i] = k.MarkerKnown
	}
	_ = failpoint.Check("pipeline:verified")
	return p.finalizeReddit(rc, it, post, dir, files, marked, nil)
}

// redditImage downloads one image to <base>.<ext> and verifies it by its
// content. A verified file from an earlier attempt is reused (invariant
// 5). gone reports a removed image (HTTP 404 or 410).
func (p *Pipeline) redditImage(ctx context.Context, rc *runCtx, dir, base, url string) (f redditFile, gone bool, err error) {
	matches, _ := filepath.Glob(filepath.Join(dir, base+".*"))
	for _, m := range matches {
		if strings.HasSuffix(m, ".part") {
			continue
		}
		if info, verr := artwork.VerifyImage(ctx, m, p.d.Env.FFprobePath); verr == nil {
			sha, size, herr := hashFile(m)
			if herr == nil {
				rc.log.Printf("Reusing verified %s", filepath.Base(m))
				return redditFile{name: filepath.Base(m), ext: info.Ext(), sha: sha, size: size}, false, nil
			}
		}
		os.Remove(m)
	}
	part := filepath.Join(dir, base+".part")
	out, err := os.Create(part)
	if err != nil {
		return f, false, failure(ytdlp.ClassTransient, err.Error())
	}
	rc.log.Printf("Downloading %s", url)
	resp, err := p.d.Reddit.Client().Download(ctx, url, "https://www.reddit.com/", out, maxImageBytes)
	if cerr := out.Close(); err == nil && cerr != nil {
		err = cerr
	}
	fail := func(class ytdlp.Class, reason string) (redditFile, bool, error) {
		os.Remove(part)
		return redditFile{}, false, failure(class, reason)
	}
	switch {
	case ctx.Err() != nil:
		os.Remove(part)
		return f, false, errStop
	case errors.Is(err, browser.ErrTooLarge):
		return fail(ytdlp.ClassPermanent, fmt.Sprintf("image larger than %d MB", maxImageBytes>>20))
	case errors.Is(err, browser.ErrNotAllowed):
		return fail(ytdlp.ClassPermanent, "the image is on a host that is not allowed: "+url)
	case err != nil:
		return fail(ytdlp.ClassTransient, "image download failed: "+err.Error())
	case resp.Status == 404 || resp.Status == 410:
		os.Remove(part)
		return f, true, nil
	case resp.Status == 429:
		return fail(ytdlp.ClassRateLimited, "rate-limited by "+hostOf(url))
	case resp.Status == 403:
		return fail(ytdlp.ClassBlocked, fmt.Sprintf("%s refused the image (HTTP 403)", hostOf(url)))
	case resp.Status != 200:
		return fail(ytdlp.ClassTransient, fmt.Sprintf("HTTP %d from %s", resp.Status, hostOf(url)))
	}
	info, err := artwork.VerifyImage(ctx, part, p.d.Env.FFprobePath)
	switch {
	case errors.Is(err, artwork.ErrNotImage):
		return fail(ytdlp.ClassPermanent, "not an image: "+url+" returned other content")
	case err != nil:
		return fail(ytdlp.ClassTransient, "verification failed: "+err.Error())
	}
	sha, size, err := hashFile(part)
	if err == nil {
		err = library.SyncPath(part)
	}
	final := filepath.Join(dir, base+"."+info.Ext())
	if err == nil {
		err = os.Rename(part, final)
	}
	if err != nil {
		return fail(ytdlp.ClassTransient, err.Error())
	}
	rc.log.Printf("Verified %s: %s %d×%d, %s", filepath.Base(final), info.Type, info.Width, info.Height, humanBytes(size))
	_ = failpoint.Check("reddit:image-saved")
	return redditFile{name: filepath.Base(final), ext: info.Ext(), sha: sha, size: size}, false, nil
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	host, _, _ := strings.Cut(u, "/")
	return host
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// setPostTime gives a media file the post time as its modification time
// (Reddit concept §8); finalize fsyncs it with the staged folder.
func setPostTime(path string, t time.Time) {
	if !t.IsZero() {
		_ = os.Chtimes(path, t, t)
	}
}

// skipDuplicate skips an item whose media is in the library already,
// writing its aliases in the same transaction (Reddit concept §7.5).
func (p *Pipeline) skipDuplicate(rc *runCtx, it *store.Item, msg string, aliases []store.MediaRow) error {
	it.State = store.ItemSkipped
	it.Error, it.ErrorClass, it.Hint = msg, "", ""
	it.FinishedAt = p.d.Now()
	if err := p.d.Store.SkipDuplicate(bg, it, aliases); err != nil {
		return failure(ytdlp.ClassTransient, "recording the duplicate failed: "+err.Error())
	}
	_ = failpoint.Check("reddit:duplicate-recorded")
	p.d.Pub.ItemChanged(it)
	rc.log.Printf("Skipped: %s", msg)
	os.RemoveAll(ItemWorkDir(p.d.Env, rc.job.ID, it.ID))
	return errSkipped
}

// redditVideo downloads a v.redd.it video through yt-dlp from the DASH
// manifest the code built from the validated id (Reddit concept §7.2
// step 5). The key comes from the post, never from yt-dlp's metadata.
func (p *Pipeline) redditVideo(ctx context.Context, rc *runCtx, it *store.Item, post *reddit.Post, dir string) error {
	rc.proxy = rc.settings.Reddit.Proxy
	defer func() { rc.proxy = "" }()
	_, meta, err := p.fetchMeta(ctx, rc, it, dir, reddit.ManifestURL(post.VideoID), false)
	if err != nil {
		return err
	}
	if meta.IsPlaylist {
		return failure(ytdlp.ClassUnknown, "the video manifest is not a single video")
	}
	it.Duration = firstPositive(meta.Duration, post.Duration)
	mediaPath, vr, err := p.downloadMedia(ctx, rc, it, dir, meta)
	if err != nil {
		return err
	}

	it.State = store.ItemExtras
	p.saveItem(it)
	p.publishStage(rc, it, "thumbnail")
	p.redditThumbnail(ctx, rc, it, post, dir, mediaPath, vr)
	if ctx.Err() != nil {
		return errStop
	}
	info, err := nfo.ParseInfo(postInfo(post, it.Duration))
	if err != nil {
		return failure(ytdlp.ClassUnknown, "building the NFO failed: "+err.Error())
	}
	if err := p.writeNFO(rc, dir, info); err != nil {
		return err
	}
	setPostTime(mediaPath, post.Created)
	_ = failpoint.Check("pipeline:extras-done")

	ext := strings.TrimPrefix(filepath.Ext(mediaPath), ".")
	files := []redditFile{{name: filepath.Base(mediaPath), ext: ext, media: it.ArchiveKey, size: vr.Size}}
	var extra []store.FileMove
	if _, err := os.Stat(filepath.Join(dir, "folder.jpg")); err == nil {
		extra = append(extra, store.FileMove{Src: "folder.jpg", Dst: "folder.jpg"})
	}
	extra = append(extra, store.FileMove{Src: "movie.nfo", Dst: "movie.nfo"})
	return p.finalizeReddit(rc, it, post, dir, files, nil, extra)
}

func firstPositive(v ...float64) float64 {
	for _, x := range v {
		if x > 0 {
			return x
		}
	}
	return 0
}

// redditThumbnail writes folder.jpg from the post's preview, fetched with
// the browser client, or from a frame of the video.
func (p *Pipeline) redditThumbnail(ctx context.Context, rc *runCtx, it *store.Item, post *reddit.Post, dir, mediaPath string, vr VerifyResult) {
	dst := filepath.Join(dir, "folder.jpg")
	if post.Thumbnail != "" {
		var buf bytes.Buffer
		resp, err := p.d.Reddit.Client().Download(ctx, post.Thumbnail, "https://www.reddit.com/", &buf, maxThumbBytes)
		switch {
		case err != nil:
			rc.log.Printf("Thumbnail: %v", err)
		case resp.Status != 200:
			rc.log.Printf("Thumbnail: HTTP %d", resp.Status)
		default:
			if err := p.d.Artwork.FromBytes(ctx, buf.Bytes(), dst); err == nil {
				return
			} else {
				rc.log.Printf("Thumbnail: %v", err)
			}
		}
	}
	if _, err := p.d.Artwork.Make(ctx, artwork.Request{MediaPath: mediaPath, Duration: vr.Duration, HasVideo: vr.Video > 0, Dst: dst}); err != nil {
		if ctx.Err() == nil {
			it.AddWarning("No thumbnail")
			rc.log.Line("WARNING: no thumbnail could be produced; the video is kept without folder.jpg")
		}
		return
	}
	rc.log.Line("Thumbnail unavailable, using a frame of the video")
}

// postInfo builds a yt-dlp-shaped info.json from a post, so the NFO
// writer stays unchanged (Reddit concept §7.2 step 6).
func postInfo(post *reddit.Post, duration float64) []byte {
	info := map[string]any{
		"id": post.ID, "title": post.Title, "description": post.Selftext, "uploader": post.Author,
		"webpage_url": postPermalink(post), "webpage_url_domain": "reddit.com",
		"extractor": "RedditPost", "extractor_key": reddit.ExtractorPost,
	}
	if post.Subreddit != "" {
		info["channel"] = "r/" + post.Subreddit
	}
	if !post.Created.IsZero() {
		info["upload_date"] = post.Created.UTC().Format("20060102")
		info["timestamp"] = post.Created.Unix()
	}
	if duration > 0 {
		info["duration"] = duration
	}
	if post.Flair != "" {
		info["tags"] = []string{post.Flair}
	}
	if post.NSFW {
		info["age_limit"] = 18
	}
	raw, _ := json.Marshal(info)
	return raw
}

func postPermalink(post *reddit.Post) string {
	if strings.HasPrefix(post.Permalink, "https://") {
		return post.Permalink
	}
	if strings.HasPrefix(post.Permalink, "/") {
		return "https://www.reddit.com" + post.Permalink
	}
	return reddit.PostURL(post.Subreddit, post.ID)
}

// finalizeReddit names the files, writes the marker and finalizes the
// item like any other (Reddit concept §8): <base>.<ext> for one file,
// "<base> - NN.<ext>" by gallery position for a gallery.
func (p *Pipeline) finalizeReddit(rc *runCtx, it *store.Item, post *reddit.Post, dir string, files []redditFile,
	known []library.MarkerKnown, extra []store.FileMove) error {
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
	created := post.Created.UTC()
	date, year := "", ""
	if !post.Created.IsZero() {
		date = created.Format("20060102")
		year = date[:4]
	}
	channel := ""
	if post.Subreddit != "" {
		channel = "r/" + post.Subreddit
	}
	rendered := tpl.Render(map[string]string{
		"title": firstNonEmpty(post.Title, it.Title, post.ID), "id": post.ID, "uploader": post.Author,
		"channel": channel, "upload_date": date, "upload_year": year,
		"playlist_title": it.PlaylistTitle, "extractor": "reddit", "ext": files[0].ext,
	})

	unlockRoot := library.LockRoot(root)
	defer unlockRoot()
	target, replace, err := library.ChooseTarget(root, rendered, it.VideoID)
	if err != nil {
		return failure(ytdlp.ClassTransient, err.Error())
	}
	if replace {
		rc.log.Printf("Replacing existing library folder of the same post: %s", target.Dir())
	}
	marker := &library.Marker{Key: it.ArchiveKey, Post: post.ID, Kind: post.Kind, Subreddit: post.Subreddit, Author: post.Author,
		Title: post.Title, Permalink: postPermalink(post), Created: created, Provider: post.Provider, Known: known}
	var moves []store.FileMove
	for _, f := range files {
		dst := target.Base + "." + f.ext
		if f.pos > 0 {
			dst = fmt.Sprintf("%s - %02d.%s", target.Base, f.pos, f.ext)
		}
		moves = append(moves, store.FileMove{Src: f.name, Dst: dst, Size: f.size, Media: f.media, SHA256: f.sha})
		marker.Files = append(marker.Files, library.MarkerFile{Name: dst, Media: f.media, SHA256: f.sha})
	}
	moves = append(moves, extra...)
	data, err := marker.Encode()
	if err == nil {
		err = library.WriteFileAtomic(filepath.Join(dir, library.MarkerName), data, 0o644)
	}
	if err != nil {
		return failure(ytdlp.ClassTransient, "writing the marker failed: "+err.Error())
	}
	moves = append(moves, store.FileMove{Src: library.MarkerName, Dst: library.MarkerName})

	// Record the plan before touching the library (step 1).
	it.State = store.ItemFinalizing
	it.Library = job.Options.Library
	it.FinalPath = target.Dir()
	it.Files = moves
	p.saveItem(it)
	p.publishStage(rc, it, "finalizing")
	return p.runFinalize(rc, it, root, dir, it.VideoID)
}

// mediaRows returns the media index rows of a finalized item: its files,
// and the identical images its marker lists as aliases.
func mediaRows(it *store.Item, root string) []store.MediaRow {
	var rows []store.MediaRow
	for _, f := range it.Files {
		if f.Media != "" {
			rows = append(rows, store.MediaRow{MediaKey: f.Media, Path: it.FinalPath + "/" + f.Dst, SHA256: f.SHA256})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	if m := library.ReadMarker(filepath.Join(root, filepath.FromSlash(it.FinalPath))); m != nil {
		for _, k := range m.Known {
			if k.SHA256 != "" {
				rows = append(rows, store.MediaRow{MediaKey: k.Media, SHA256: k.SHA256, Alias: true})
			}
		}
	}
	return rows
}

// redditPlan creates the items of a pasted Reddit URL without a yt-dlp
// probe (Reddit concept §7.2): a post becomes one item, a subreddit or
// user listing a one-off job with jobListLimit posts in the URL's sort
// order. Text and link posts are left out.
func (p *Pipeline) redditPlan(ctx context.Context, rc *runCtx, t reddit.Target) (scheduler.Outcome, bool) {
	job := rc.job
	fail := func(class ytdlp.Class, reason string) (scheduler.Outcome, bool) {
		return p.failJob(rc, ytdlp.Classification{Class: class, Reason: reason}), false
	}
	if t.Kind == reddit.TargetShare {
		rc.log.Printf("Resolving share link %s", t.URL())
		resp, err := p.d.Reddit.Client().Document(ctx, t.URL(), "https://www.reddit.com/", 8<<20)
		if ctx.Err() != nil {
			return p.stop(ctx, rc), false
		}
		if err != nil {
			return p.jobFailure(ctx, rc, ytdlp.Classification{Class: ytdlp.ClassTransient, Reason: "resolving the share link failed: " + err.Error()}), false
		}
		resolved, ok := reddit.Parse(resp.URL)
		if !ok || resolved.Kind != reddit.TargetPost {
			return fail(ytdlp.ClassPermanent, "the share link does not lead to a post")
		}
		t = resolved
	}
	var items []*store.Item
	switch {
	case t.Kind == reddit.TargetPost:
		items = append(items, &store.Item{JobID: job.ID, URL: reddit.PostURL(t.Subreddit, t.PostID), Extractor: reddit.ExtractorPost,
			VideoID: t.PostID, ArchiveKey: reddit.PostKey(t.PostID), Title: t.PostID, State: store.ItemPending})
	case t.IsListing():
		limit := min(max(rc.settings.Reddit.JobListLimit, 1), reddit.MaxListing)
		rc.log.Printf("Listing %s (%d posts, %s)", t.Name(), limit, t.EffectiveSort())
		page, provider, rerr := p.redditChain(rc).List(ctx, reddit.Query{Target: t, Limit: limit})
		if ctx.Err() != nil {
			return p.stop(ctx, rc), false
		}
		if rerr != nil {
			return p.jobFailure(ctx, rc, ytdlp.Classification{Class: rerr.Class, Reason: rerr.Reason, Hint: rerr.Hint, Norm: ytdlp.Normalize(rerr.Reason)}), false
		}
		job.Title, job.IsPlaylist = t.Name(), true
		if page.Icon != "" {
			job.Thumbnail = page.Icon
		}
		seen := map[string]bool{}
		ignored, skipped := 0, 0
		for i := range page.Posts {
			post := &page.Posts[i]
			if len(items) >= limit {
				break
			}
			if post.Kind == reddit.KindText || post.Kind == reddit.KindLink {
				ignored++
				continue
			}
			ext, id := post.ListingKey()
			key := strings.ToLower(ext) + " " + id
			if seen[key] {
				continue
			}
			seen[key] = true
			it := &store.Item{JobID: job.ID, Index: len(items), URL: postPermalink(post), Extractor: ext, VideoID: id,
				ArchiveKey: key, Title: firstNonEmpty(post.Title, post.ID), PlaylistTitle: t.Folder(), State: store.ItemPending}
			if !job.Options.Force && p.inArchive(key) {
				it.State, it.Error, it.FinishedAt = store.ItemSkipped, "Already in library", p.d.Now()
				skipped++
			}
			items = append(items, it)
		}
		rc.log.Printf("Found %d posts with media in %s via %s (%d without media left out, %d already in library)",
			len(items), t.Name(), provider, ignored, skipped)
		if len(items) == 0 {
			return fail(ytdlp.ClassPermanent, "no posts with images or videos found")
		}
	default:
		return fail(ytdlp.ClassPermanent, "not a Reddit post, subreddit or user URL")
	}
	if err := p.d.Store.CreateItems(bg, items); err != nil {
		return fail(ytdlp.ClassTransient, err.Error())
	}
	_ = failpoint.Check("pipeline:probed")
	p.saveJob(job)
	return scheduler.Outcome{}, true
}
