package ytdlp

import (
	"slices"
	"strings"
	"testing"
)

// Corpus of yt-dlp ERROR/WARNING lines (anonymized: no URLs, titles or IDs).
var corpus = []struct {
	line  string
	class Class
	media bool
}{
	// The extension-allowlist rejection also contains the generic "please
	// report" text, but must be permanent, not extractor_broken (F21).
	{"ERROR: The extracted extension ('v1712345678') is unusual and will be skipped for safety reasons. If you believe this is an error, please report this issue on <issues>, filling out the appropriate issue template. Confirm you are on the latest version using  yt-dlp -U", ClassPermanent, false},
	{"ERROR: [example] abc123: Unable to download webpage: HTTP Error 403: Forbidden (caused by <HTTPError 403: Forbidden>)", ClassBlocked, false},
	{"ERROR: [example] abc123: Unable to download JSON metadata: HTTP Error 403: Forbidden", ClassBlocked, false},
	{"ERROR: [generic] Got HTTP Error 403 caused by Cloudflare anti-bot challenge; try again with --extractor-args \"generic:impersonate\"", ClassBlocked, false},
	{"ERROR: unable to download video data: HTTP Error 403: Forbidden", ClassBlocked, true},
	{"ERROR: [example] abc123: Unable to download webpage: HTTP Error 429: Too Many Requests (caused by <HTTPError 429: Too Many Requests>)", ClassRateLimited, false},
	{"ERROR: [example] abc123: This content isn't available, try again later. Your account has been rate-limited", ClassRateLimited, false},
	{"ERROR: [example] abc123: Private video. Sign in if you've been granted access to this video", ClassPermanent, false},
	{"ERROR: [example] abc123: Video unavailable. This video has been removed by the uploader", ClassPermanent, false},
	{"ERROR: [example] abc123: Video unavailable. The uploader has not made this video available in your country", ClassPermanent, false},
	{"ERROR: [example] abc123: This video is DRM protected", ClassPermanent, false},
	{"ERROR: Unsupported URL: <url>", ClassPermanent, false},
	{"ERROR: [example] abc123: Requested format is not available. Use --list-formats for a list of available formats", ClassPermanent, false},
	{"ERROR: [example] abc123: Unable to download webpage: HTTP Error 404: Not Found (caused by <HTTPError 404: Not Found>)", ClassPermanent, false},
	{"ERROR: [example] abc123: Premieres in 3 hours", ClassPermanent, false},
	{"ERROR: [example] abc123: Sign in to confirm your age. This video may be inappropriate for some users.", ClassAuthRequired, false},
	{"ERROR: [example] abc123: Sign in to confirm you’re not a bot. Use --cookies-from-browser or --cookies for the authentication.", ClassAuthRequired, false},
	{"ERROR: [example] abc123: Join this channel to get access to members-only content like this video", ClassAuthRequired, false},
	{"ERROR: [example] abc123: This video is only available for registered users. Use --cookies, --cookies-from-browser, --username and --password, --netrc-cmd, or --netrc (example) to provide account credentials", ClassAuthRequired, false},
	{"ERROR: [example] abc123: Unable to extract video data; please report this issue on <issues>, filling out the appropriate issue template. Confirm you are on the latest version using  yt-dlp -U", ClassExtractorBroken, false},
	{"ERROR: [example] abc123: KeyError('streamingData'); please report this issue on <issues>", ClassExtractorBroken, false},
	{"ERROR: [example] abc123: Unable to download webpage: <urlopen error [Errno -3] Temporary failure in name resolution> (caused by TransportError(...))", ClassTransient, false},
	{"ERROR: [example] abc123: Unable to download webpage: The read operation timed out", ClassTransient, false},
	{"ERROR: unable to download video data: HTTP Error 503: Service Unavailable", ClassTransient, false},
	{"ERROR: fragment 17 not found, unable to continue", ClassTransient, false},
	{"ERROR: Did not get any data blocks", ClassTransient, false},
	{"ERROR: [Errno 104] Connection reset by peer", ClassTransient, false},
	{"ERROR: Impersonate target \"chrome\" is not available. Use --list-impersonate-targets to see available targets.", ClassPermanent, false},
	{"ERROR: Postprocessing: Something nobody has seen before", ClassUnknown, false},
}

func TestClassifierCorpus(t *testing.T) {
	for _, c := range corpus {
		got := ClassifyMessage(c.line)
		if got.Class != c.class || got.MediaForbidden != c.media {
			t.Errorf("%q\n  got %s (media=%v), want %s (media=%v)", c.line, got.Class, got.MediaForbidden, c.class, c.media)
		}
		if got.Reason == "" || got.Hint == "" || got.Norm == "" {
			t.Errorf("%q: empty reason/hint/norm: %+v", c.line, got)
		}
	}
}

func TestClassifyResult(t *testing.T) {
	// Traceback without ERROR line -> crash (F23).
	res := &Result{ExitCode: 1, Traceback: true, Tail: []string{
		"Traceback (most recent call last):", `  File "x.py", line 1, in <module>`, "TypeError: argument of type 'NoneType' is not iterable"}}
	c := Classify(res)
	if c.Class != ClassCrash || !strings.Contains(c.Reason, "NoneType") {
		t.Fatalf("got %+v", c)
	}
	// Killed by signal.
	if c := Classify(&Result{ExitCode: -1, Signal: "killed"}); c.Class != ClassCrash {
		t.Fatalf("got %+v", c)
	}
	// Unexpected non-zero exit without ERROR line.
	if c := Classify(&Result{ExitCode: 2}); c.Class != ClassCrash {
		t.Fatalf("got %+v", c)
	}
	// Generic ERROR, 429 in a warning -> rate_limited.
	res = &Result{ExitCode: 1, Errors: []string{"ERROR: something odd happened"},
		Warnings: []string{"WARNING: [example] HTTP Error 429: Too Many Requests"}}
	if c := Classify(res); c.Class != ClassRateLimited {
		t.Fatalf("got %+v", c)
	}
	// The most specific line wins.
	res = &Result{ExitCode: 1, Errors: []string{"ERROR: Private video", "ERROR: [Errno 104] Connection reset by peer"}}
	if c := Classify(res); c.Class != ClassPermanent {
		t.Fatalf("got %+v", c)
	}
	if c := Classify(&Result{}); c.Class != ClassNone {
		t.Fatalf("success classified as %s", c.Class)
	}
}

func TestNormalize(t *testing.T) {
	a := Normalize("ERROR: [example] abc123: Unable to download webpage: HTTP Error 500 (attempt 3 of 10) https://x/y?z=1")
	b := Normalize("ERROR: [example] zzz999: Unable to download webpage: HTTP Error 500 (attempt 7 of 10) https://x/q")
	if a != b {
		t.Fatalf("%q != %q", a, b)
	}
}

func TestImpersonationWarning(t *testing.T) {
	lines := []string{"WARNING: [generic] The extractor is attempting impersonation, but no impersonate target is available; if you encounter errors, then see  <docs>  for information on installing the required dependencies"}
	if !ImpersonationUnavailable(lines) {
		t.Fatal("warning not detected")
	}
}

func TestParseProgress(t *testing.T) {
	p, ok := ParseProgress(`@@P {"status": "downloading", "downloaded_bytes": 512, "total_bytes": 2048, "speed": 1000.5, "eta": 3, "filename": "/w/media.f1.mp4"}`)
	if !ok || p.Percent() != 25 || p.Speed != 1000.5 || p.ETA != 3 {
		t.Fatalf("got %+v ok=%v", p, ok)
	}
	p, ok = ParseProgress(`@@P {"status": "downloading", "downloaded_bytes": 10, "total_bytes": "NA", "total_bytes_estimate": 100, "fragment_index": 1, "fragment_count": 4}`)
	if !ok || p.Percent() != 10 {
		t.Fatalf("estimate: %+v", p)
	}
	if _, ok := ParseProgress("[download] 50% of 10MiB"); ok {
		t.Fatal("plain line parsed as progress")
	}
}

func TestParseTargets(t *testing.T) {
	out := `[info] Available impersonate targets
Client          OS           Source
--------------------------------------
Chrome-133      Macos-15     curl_cffi
Safari-17.2     Ios-17.2     curl_cffi
Edge            Windows-10   curl_cffi (unavailable)
`
	ts := ParseTargets(out)
	if len(ts) != 3 || ts[0].Name() != "chrome-133:macos-15" || !ts[0].Available || ts[2].Available {
		t.Fatalf("got %+v", ts)
	}
}

func TestArgsSafety(t *testing.T) {
	args := ProbeArgs("https://example.com/v", CallOptions{Impersonate: "any", CookiesFile: "/w/.cookies.txt"})
	if args[0] != "--ignore-config" || args[len(args)-2] != "--" || args[len(args)-1] != "https://example.com/v" {
		t.Fatalf("url must follow --: %v", args)
	}
	if !slices.Contains(args, "--impersonate=") || !slices.Contains(args, "--flat-playlist") {
		t.Fatalf("missing flags: %v", args)
	}
	d := DownloadArgs(DownloadSpec{WorkDir: "/w", Format: "bv*+ba/best", Container: "mkv"}, CallOptions{})
	for _, bad := range []string{"--write-thumbnail", "--write-subs", "--compat-options", "--exec"} {
		if slices.Contains(d, bad) {
			t.Fatalf("download must not contain %s: %v", bad, d)
		}
	}
	if !slices.Contains(d, "--abort-on-unavailable-fragments") || !slices.Contains(d, "--load-info-json") {
		t.Fatalf("missing flags: %v", d)
	}
	s := SubtitleArgs("/w", false, CallOptions{})
	if !slices.Contains(s, "all,-live_chat") || !slices.Contains(s, "--skip-download") {
		t.Fatalf("subtitle args: %v", s)
	}
}
