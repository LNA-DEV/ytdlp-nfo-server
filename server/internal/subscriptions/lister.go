package subscriptions

import (
	"context"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp/versions"
)

// ListRequest is one listing of a source or of a preview URL.
type ListRequest struct {
	URL     string // a source's canonical URL, or the URL of a preview
	Tab     string // the source's YouTube tab, "" otherwise
	Window  int    // newest entries to list; 0 = the deepest listing the lister allows
	Timeout time.Duration
	Bin     versions.Binary // the yt-dlp build of the check
	Call    ytdlp.CallOptions
}

// ListResult is the outcome of a listing.
type ListResult struct {
	Listing *ytdlp.Listing // nil on failure
	// Complete: the listing holds every entry of the source.
	Complete bool
	// Exhausted: the lister can't list deeper, so a larger window would
	// return the same entries.
	Exhausted bool
	TimedOut  bool
	Output    []string // tool output, for the check log on failure
}

// Lister lists sources (Reddit concept §7.1). The check loop and the
// preview pick one per URL.
type Lister interface {
	List(ctx context.Context, req ListRequest) (*ListResult, *ytdlp.Classification)
}

// YtdlpLister lists with a flat yt-dlp listing (Appendix A).
type YtdlpLister struct{}

// List runs the listing. A full listing (window 0) is complete.
func (YtdlpLister) List(ctx context.Context, req ListRequest) (*ListResult, *ytdlp.Classification) {
	res := &ListResult{}
	lctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	run := ytdlp.Run(lctx, ytdlp.Call{Bin: req.Bin.Path, CaptureStdout: true,
		Args:   ytdlp.ListArgs(req.URL, ytdlp.ListSpec{Window: req.Window, YouTubeTab: req.Tab != ""}, req.Call),
		OnLine: func(line string) { res.Output = append(res.Output, line) }})
	res.TimedOut = lctx.Err() != nil && ctx.Err() == nil
	if !run.OK() {
		cls := ListingClass(ytdlp.Classify(run))
		if res.TimedOut {
			cls = ytdlp.Classification{Class: ytdlp.ClassTransient, Reason: "listing timed out after " + req.Timeout.String()}
		}
		return res, &cls
	}
	l, err := ytdlp.ParseListing(run.Stdout)
	if err != nil {
		return res, &ytdlp.Classification{Class: ytdlp.ClassUnknown, Reason: err.Error()}
	}
	res.Listing = l
	res.Complete = req.Window == 0 || len(l.Entries) < req.Window
	res.Exhausted = req.Window == 0
	return res, nil
}
