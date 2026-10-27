package ytdlp

import (
	"regexp"
	"strconv"
	"strings"
)

// Class is an error class (§4.7).
type Class string

const (
	ClassNone            Class = ""
	ClassPermanent       Class = "permanent"
	ClassExtractorBroken Class = "extractor_broken"
	ClassAuthRequired    Class = "auth_required"
	ClassBlocked         Class = "blocked"
	ClassRateLimited     Class = "rate_limited"
	ClassCrash           Class = "crash"
	ClassTransient       Class = "transient"
	ClassUnknown         Class = "unknown"
)

// Classification describes why an attempt failed.
type Classification struct {
	Class  Class
	Reason string // short human-readable reason
	Hint   string // what the user can do
	Norm   string // normalized message for the deterministic-error guard
	// MediaForbidden is set for HTTP 403 on the media URL itself: signed
	// URLs may have expired, so the pipeline re-extracts once first.
	MediaForbidden bool
}

type rule struct {
	class  Class
	re     *regexp.Regexp
	reason string
	hint   string
	media  bool
}

func r(class Class, pattern, reason, hint string) rule {
	return rule{class: class, re: regexp.MustCompile(`(?i)` + pattern), reason: reason, hint: hint}
}

// ReasonNotYetAvailable is the reason of a premiere or an upcoming live
// stream. Subscriptions treat an item that failed with it as upcoming
// (subscriptions concept §5.3), so it must stay stable.
const ReasonNotYetAvailable = "video is not available yet (premiere or upcoming live stream)"

// Hints shown in the UI per class.
const (
	HintPermanent       = "Not retried automatically: this error does not go away by waiting."
	HintExtractorBroken = "Site support may be broken in this yt-dlp version. Try \"Retry with latest nightly\"."
	HintAuthRequired    = "Needs cookies: retry with a cookie profile."
	HintBlocked         = "Access denied by the site: try impersonation, cookies or a different network."
	HintRateLimited     = "Rate-limited by the site."
	HintCrash           = "yt-dlp crashed. Try another yt-dlp version."
)

// Specific patterns come before generic ones: the extension-allowlist
// message also contains yt-dlp's generic "please report this issue" text
// but is permanent, not extractor_broken.
var rules = []rule{
	r(ClassPermanent, `extracted extension .* is unusual|will be skipped for safety reasons|unsafe file extension`,
		"file extension rejected by yt-dlp's safety allowlist",
		"Not retried automatically. The media file type is not on yt-dlp's allowlist; try a newer yt-dlp version."),
	r(ClassPermanent, `impersonate target .*(is not available|not available)|no impersonate targets? (is |are )?available`,
		"impersonation target not available in this yt-dlp build",
		"Choose another impersonation target or a yt-dlp build with curl_cffi."),
	r(ClassAuthRequired, `sign in to confirm (your age|you.?re not a bot|you are not a bot)|confirm you.?re not a bot`,
		"the site requires a signed-in session (bot or age check)", HintAuthRequired),
	r(ClassAuthRequired, `age[- ]restricted|inappropriate for some users|age verification`,
		"age verification required", HintAuthRequired),
	r(ClassAuthRequired, `members[- ]only|join this channel|available to this channel's members|requires (a )?(paid )?subscription|requires payment|purchase`,
		"members-only or paid content", HintAuthRequired),
	r(ClassAuthRequired, `(login|log in|sign in|logged in|authentication|account) (is )?(required|needed)|use --cookies|--cookies-from-browser|only available for registered users|needs? (to )?(log|sign)[ -]?in`,
		"login required", HintAuthRequired),
	r(ClassRateLimited, `HTTP Error 429|too many requests|rate[- ]?limit`, "rate-limited by the site", HintRateLimited),
	{class: ClassBlocked, re: regexp.MustCompile(`(?i)unable to download video data: HTTP Error 403|(fragment|video data).*403: Forbidden`),
		reason: "access to the media URL was denied (HTTP 403)", hint: HintBlocked, media: true},
	r(ClassBlocked, `HTTP Error 403|403: Forbidden|anti-bot|cloudflare|captcha|bot (check|detection|protection)|access denied|--impersonate|generic:impersonate`,
		"access denied by the site", HintBlocked),
	r(ClassTransient, `fragment \d+ not found|unable to download fragment|giving up after \d+ fragment|did not get any data blocks`,
		"fragment download failed", ""),
	r(ClassPermanent, `private video|video is private|this video is private|private (playlist|account)`, "video is private", HintPermanent),
	r(ClassPermanent, `\bDRM\b|drm protected|widevine|playready`, "video is DRM-protected", HintPermanent),
	r(ClassPermanent, `not available (in|from) your (country|location|region)|geo[- ]?restrict|blocked in your country|uploader has not made this video available in your country`,
		"video is geo-restricted", HintPermanent),
	r(ClassPermanent, `copyright`, "video is blocked on copyright grounds", HintPermanent),
	r(ClassPermanent, `unsupported url|is not a valid url|not a valid url`, "unsupported URL", HintPermanent),
	r(ClassPermanent, `no video formats found|requested format is not available|no formats? (found|available)`,
		"no downloadable formats", HintPermanent),
	r(ClassPermanent, `premieres in|live event will begin|this live event will|is not yet available|is upcoming`,
		ReasonNotYetAvailable, HintPermanent),
	r(ClassPermanent, `video unavailable|this video is (no longer )?(un)?available|has been removed|was removed|been terminated|no longer available|does not exist|HTTP Error 404|HTTP Error 410|404: Not Found|content (is )?not available`,
		"video is unavailable or was removed", HintPermanent),
	r(ClassExtractorBroken, `unable to extract|unable to parse|failed to parse|please report this issue|latest version using\s+yt-dlp -U|is this a bug|signature extraction failed|nsig extraction failed|challenge solving failed|could not find js function|unsupported (player|js)`,
		"site support may be broken in this yt-dlp version", HintExtractorBroken),
	r(ClassTransient, `timed? ?out|connection (reset|refused|aborted|closed)|temporary failure|name resolution|network is unreachable|no route to host|HTTP Error 5\d\d|IncompleteRead|remote end closed|\bssl\b|eof occurred|unable to download (webpage|json|api)|giving up after|broken pipe|no space left|errno \d+|read error`,
		"network error", ""),
}

var (
	prefixRe = regexp.MustCompile(`^(ERROR|WARNING):\s*(\[[^\]]+\]\s*)?`)
	idRe     = regexp.MustCompile(`^[^\s:]{1,64}:\s+`)
	urlRe    = regexp.MustCompile(`https?://\S+`)
	digitRe  = regexp.MustCompile(`[0-9]+`)
	spaceRe  = regexp.MustCompile(`\s+`)
)

// CleanMessage strips "ERROR: [extractor] id:" prefixes.
func CleanMessage(line string) string {
	m := prefixRe.FindStringSubmatch(line)
	s := line
	if m != nil {
		s = line[len(m[0]):]
		if m[2] != "" {
			s = idRe.ReplaceAllString(s, "")
		}
	}
	return strings.TrimSpace(s)
}

// Normalize returns a message with URLs, numbers and spacing collapsed, used
// to detect two consecutive identical failures.
func Normalize(msg string) string {
	s := strings.ToLower(CleanMessage(msg))
	s = urlRe.ReplaceAllString(s, "<url>")
	s = digitRe.ReplaceAllString(s, "#")
	s = spaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
		cut = cut[:len(cut)-1]
	}
	if len(cut) > 0 && cut[len(cut)-1] >= 0xC0 {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// ClassifyMessage classifies a single error message.
func ClassifyMessage(msg string) Classification {
	clean := CleanMessage(msg)
	for _, ru := range rules {
		if ru.re.MatchString(msg) {
			c := Classification{Class: ru.class, Reason: ru.reason, Hint: ru.hint, Norm: Normalize(msg), MediaForbidden: ru.media}
			if c.Reason == "" || c.Class == ClassTransient {
				c.Reason = truncate(clean, 200)
			}
			if c.Hint == "" {
				c.Hint = truncate(clean, 300)
			}
			return c
		}
	}
	return Classification{Class: ClassUnknown, Reason: truncate(clean, 200), Hint: truncate(clean, 300), Norm: Normalize(msg)}
}

// Classify classifies a failed call from its ERROR lines, warnings, exit
// status and traceback. It returns ClassNone for a successful call.
func Classify(res *Result) Classification {
	if res.OK() {
		return Classification{}
	}
	if res.Interrupted {
		return Classification{Class: ClassTransient, Reason: "interrupted", Norm: "interrupted"}
	}
	if res.StartErr != nil {
		return Classification{Class: ClassCrash, Reason: "yt-dlp could not be started: " + res.StartErr.Error(), Hint: HintCrash, Norm: "start error"}
	}
	if len(res.Errors) > 0 {
		// Prefer the most specific of all error lines, then the last one.
		best := ClassifyMessage(res.Errors[len(res.Errors)-1])
		for _, e := range res.Errors {
			c := ClassifyMessage(e)
			if rank(c.Class) < rank(best.Class) {
				best = c
			}
		}
		if best.Class == ClassUnknown {
			// Warnings often carry the actual cause (e.g. 429 before a
			// generic "unable to download" error).
			for _, w := range res.Warnings {
				if c := ClassifyMessage(w); c.Class == ClassRateLimited || c.Class == ClassAuthRequired || c.Class == ClassBlocked {
					c.Reason, c.Norm = best.Reason, best.Norm
					return c
				}
			}
			if res.Traceback {
				return Classification{Class: ClassCrash, Reason: "yt-dlp crashed: " + best.Reason, Hint: HintCrash, Norm: best.Norm}
			}
		}
		return best
	}
	reason := "yt-dlp exited unexpectedly without an error message"
	if res.Signal != "" {
		reason = "yt-dlp was killed by signal " + res.Signal
	} else if res.Traceback {
		reason = "yt-dlp crashed with a Python traceback"
		for i := len(res.Tail) - 1; i >= 0; i-- {
			if l := strings.TrimSpace(res.Tail[i]); l != "" && !strings.HasPrefix(l, "File ") {
				reason += ": " + truncate(l, 200)
				break
			}
		}
	} else if res.ExitCode != 0 {
		reason += " (exit status " + strconv.Itoa(res.ExitCode) + ")"
	}
	return Classification{Class: ClassCrash, Reason: reason, Hint: HintCrash, Norm: Normalize(reason)}
}

// rank orders classes from most to least specific.
func rank(c Class) int {
	switch c {
	case ClassPermanent:
		return 0
	case ClassAuthRequired:
		return 1
	case ClassRateLimited:
		return 2
	case ClassBlocked:
		return 3
	case ClassExtractorBroken:
		return 4
	case ClassTransient:
		return 5
	case ClassCrash:
		return 6
	}
	return 7
}

// impersonationUnavailableRe matches yt-dlp's warning when an extractor
// wants impersonation but curl_cffi targets are missing (F5c).
var impersonationUnavailableRe = regexp.MustCompile(`(?i)attempting impersonation, but no impersonate target is available|no impersonate target is available`)

// ImpersonationUnavailable reports whether output shows that impersonation
// was requested but could not be used.
func ImpersonationUnavailable(lines []string) bool {
	for _, l := range lines {
		if impersonationUnavailableRe.MatchString(l) {
			return true
		}
	}
	return false
}
