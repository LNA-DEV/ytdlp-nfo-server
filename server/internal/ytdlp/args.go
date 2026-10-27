// Package ytdlp builds yt-dlp command lines, runs yt-dlp, parses its
// progress output and classifies its errors.
package ytdlp

import (
	"path/filepath"
	"strconv"
)

// Files inside an item work dir.
const (
	InfoFile   = "info.json"
	ResultFile = ".result"
	MediaBase  = "media"
	TmpDir     = ".tmp"
	CookieFile = ".cookies.txt"
)

// ProgressPrefix marks progress-template lines on stdout.
const ProgressPrefix = "@@P "

// CallOptions are per-call request options.
type CallOptions struct {
	Impersonate   string // "" off, "any" -> --impersonate="", or a target
	CookiesFile   string
	LimitRate     string
	SleepRequests float64
	SleepInterval float64
	CacheDir      string
}

// NetworkArgs replace the dead /etc/yt-dlp.conf (F6): explicit retry
// behaviour for short network hiccups inside yt-dlp (§4.7 level 1).
func NetworkArgs() []string {
	return []string{
		"--retries", "10",
		"--fragment-retries", "10",
		"--extractor-retries", "5",
		"--socket-timeout", "30",
		"--retry-sleep", "http:exp=1:30",
		"--retry-sleep", "fragment:exp=1:30",
	}
}

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// common returns flags shared by every call. --ignore-config makes behaviour
// depend only on the server's settings (§11).
func (o CallOptions) common() []string {
	args := []string{"--ignore-config", "--color", "no_color"}
	if o.CacheDir != "" {
		args = append(args, "--cache-dir", o.CacheDir)
	}
	switch o.Impersonate {
	case "":
	case "any":
		args = append(args, "--impersonate=")
	default:
		args = append(args, "--impersonate="+o.Impersonate)
	}
	if o.CookiesFile != "" {
		args = append(args, "--cookies", o.CookiesFile)
	}
	if o.SleepRequests > 0 {
		args = append(args, "--sleep-requests", fmtFloat(o.SleepRequests))
	}
	return append(args, NetworkArgs()...)
}

// ProbeArgs detects single video vs playlist and enumerates entries.
func ProbeArgs(url string, o CallOptions) []string {
	args := append(o.common(), "--flat-playlist", "-J")
	return append(args, "--", url)
}

// ListSpec describes a subscription listing (subscriptions concept,
// Appendix A).
type ListSpec struct {
	// Window > 0 lists only the newest Window entries of a newest-first
	// source (--lazy-playlist -I :W); 0 is a full listing.
	Window int
	// YouTubeTab adds the fixed, server-controlled extractor argument that
	// makes YouTube tab listings report an approximate upload date.
	YouTubeTab bool
}

// ListArgs lists the entries of a channel tab or playlist.
func ListArgs(url string, l ListSpec, o CallOptions) []string {
	args := append(o.common(), "--flat-playlist", "-J")
	if l.Window > 0 {
		args = append(args, "--lazy-playlist", "-I", ":"+strconv.Itoa(l.Window))
	}
	if l.YouTubeTab {
		args = append(args, "--extractor-args", "youtubetab:approximate_date")
	}
	return append(args, "--", url)
}

// MetaArgs fetches full metadata for one entry, just in time. If the entry
// turns out to be a playlist, --flat-playlist keeps the listing cheap.
func MetaArgs(url string, o CallOptions) []string {
	args := append(o.common(), "--no-playlist", "--flat-playlist", "-J")
	return append(args, "--", url)
}

// DownloadSpec describes the media-only download of one item.
type DownloadSpec struct {
	WorkDir          string
	Format           string
	AudioMultistream bool
	Sort             string
	Container        string // mkv | mp4
	AudioOnly        bool   // the source has no video (nothing is remuxed)
	EmbedChapters    bool
	SponsorBlockMark bool
}

// DownloadArgs downloads the media only: no thumbnail and no subtitles, so
// a problem with those can never abort the media download (F21). It uses
// the stored info.json, so there is no re-extraction (F16).
func DownloadArgs(s DownloadSpec, o CallOptions) []string {
	args := append(o.common(), "--no-playlist",
		"--load-info-json", filepath.Join(s.WorkDir, InfoFile),
		"-f", s.Format)
	if s.AudioMultistream {
		args = append(args, "--audio-multistreams")
	}
	if s.Sort != "" {
		args = append(args, "-S", s.Sort)
	}
	args = append(args, "--merge-output-format", s.Container)
	if s.Container == "mkv" && !s.AudioOnly {
		// A single-file format is not merged: remux it, so every video
		// ends up in the requested container. Only mkv, which takes any
		// codec; remuxing into mp4 can fail.
		args = append(args, "--remux-video", "mkv")
	}
	args = append(args,
		"-P", "home:"+s.WorkDir,
		"-P", "temp:"+filepath.Join(s.WorkDir, TmpDir),
		"-o", MediaBase+".%(ext)s",
		"--abort-on-unavailable-fragments",
		"--no-write-thumbnail", "--no-write-subs", "--no-write-info-json",
		"--newline",
		"--progress-template", "download:"+ProgressPrefix+"%(progress)j",
		"--print-to-file", "after_move:%(filepath)s\t%(format_id)s", filepath.Join(s.WorkDir, ResultFile),
	)
	if s.EmbedChapters {
		args = append(args, "--embed-chapters")
	}
	if s.SponsorBlockMark {
		args = append(args, "--sponsorblock-mark", "all")
	}
	if o.LimitRate != "" {
		args = append(args, "--limit-rate", o.LimitRate)
	}
	if o.SleepInterval > 0 {
		args = append(args, "--sleep-interval", fmtFloat(o.SleepInterval))
	}
	return args
}

// SubtitleArgs fetches subtitles in a separate best-effort call. Live chat
// is excluded (F18).
func SubtitleArgs(workDir string, convertSRT bool, o CallOptions) []string {
	args := append(o.common(), "--no-playlist",
		"--load-info-json", filepath.Join(workDir, InfoFile),
		"--skip-download", "--write-subs", "--sub-langs", "all,-live_chat",
		"-P", "home:"+workDir,
		"-P", "temp:"+filepath.Join(workDir, TmpDir),
		"-o", MediaBase+".%(ext)s",
	)
	if convertSRT {
		args = append(args, "--convert-subs", "srt")
	}
	return args
}

// VersionArgs prints the version.
func VersionArgs() []string { return []string{"--ignore-config", "--version"} }

// ListTargetsArgs lists impersonation targets.
func ListTargetsArgs() []string { return []string{"--ignore-config", "--list-impersonate-targets"} }

// SimulateArgs runs the optional smoke-test against a URL.
func SimulateArgs(url string) []string {
	return append(append([]string{"--ignore-config", "--color", "no_color", "--simulate", "--no-playlist"}, NetworkArgs()...), "--", url)
}
