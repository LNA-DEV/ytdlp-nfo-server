// Package probe is the `reddit-probe` diagnostics command (Reddit concept
// §13): it runs the providers from the server's network and shows what
// Reddit and Arctic Shift really answer, optionally saving the pages as
// anonymized test fixtures.
package probe

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/arcticshift"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/jschallenge"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/native"
)

// Test hooks (fakereddit).
var (
	testRoots *x509.CertPool
	testDial  func(ctx context.Context, network, addr string) (net.Conn, error)
)

type response struct {
	url    string
	status int
	body   []byte
}

type recorder struct {
	mu   sync.Mutex
	resp []response
}

func (r *recorder) add(u string, status int, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resp = append(r.resp, response{u, status, append([]byte(nil), body...)})
}

func (r *recorder) take() []response {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.resp
	r.resp = nil
	return out
}

// Main runs the command; it returns the exit code.
func Main(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("reddit-probe", flag.ContinueOnError)
	fs.SetOutput(stdout)
	provider := fs.String("provider", "all", "native, arcticshift or all")
	client := fs.String("client", "browser", "browser (the Chrome identity) or plain (Go's HTTP client)")
	proxy := fs.String("proxy", os.Getenv("REDDIT_PROXY"), "http://, https:// or socks5:// proxy")
	limit := fs.Int("limit", 25, "posts to list")
	fixtures := fs.String("fixtures", "", "save the responses, anonymized, in this directory")
	quiet := fs.Bool("quiet", false, "one line per run (for cron)")
	solve := fs.Bool("solve", false, "answer Reddit's JavaScript check, as the setting solveChallenge does")
	fs.Usage = func() {
		fmt.Fprintln(stdout, "usage: ytdlp-nfo-server reddit-probe [flags] <subreddit, user or post URL>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	target, ok := reddit.Parse(fs.Arg(0))
	if !ok || (!target.IsListing() && target.Kind != reddit.TargetPost) {
		fmt.Fprintln(stdout, "not a Reddit subreddit, user or post URL:", fs.Arg(0))
		return 2
	}
	if *client != "browser" && *client != "plain" {
		fmt.Fprintln(stdout, "unknown client", *client)
		return 2
	}
	var rec recorder
	c, err := browser.New(browser.Options{Proxy: *proxy, Plain: *client == "plain", OnResponse: rec.add,
		RootCAs: testRoots, Dial: testDial})
	if err != nil {
		fmt.Fprintln(stdout, err)
		return 2
	}
	defer c.Close()
	nat := native.New(c)
	nat.PageDelay = func() time.Duration { return 2 * time.Second }
	if *solve {
		nat.Solve = jschallenge.Solver{UserAgent: browser.Chrome.UserAgent}.Solve
	}
	arc := arcticshift.New(c)
	if testDial != nil {
		nat.PageDelay, nat.MinGap, arc.MinInterval = nil, 0, 0
	}
	var providers []reddit.Provider
	switch *provider {
	case "native":
		providers = []reddit.Provider{nat}
	case "arcticshift":
		providers = []reddit.Provider{arc}
	case "all":
		providers = []reddit.Provider{nat, arc}
	default:
		fmt.Fprintln(stdout, "unknown provider", *provider)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var summary []string
	failed := false
	for _, p := range providers {
		posts, perr := run(ctx, p, target, *limit)
		resps := rec.take()
		media, mediaOK := "", true
		if perr == nil {
			media, mediaOK = checkMedia(ctx, c, posts)
			rec.take()
		}
		if *fixtures != "" {
			if err := save(*fixtures, p.Name(), resps); err != nil {
				fmt.Fprintln(stdout, "fixtures:", err)
			}
		}
		if perr != nil {
			failed = true
			summary = append(summary, fmt.Sprintf("%s=%s(%s)", p.Name(), perr.Class, perr.Reason))
		} else if !mediaOK {
			failed = true
			summary = append(summary, fmt.Sprintf("%s=media(%d posts%s)", p.Name(), len(posts), media))
		} else {
			summary = append(summary, fmt.Sprintf("%s=ok(%d posts%s)", p.Name(), len(posts), media))
		}
		if *quiet {
			continue
		}
		report(stdout, p.Name(), *client, resps, posts, perr, media)
	}
	if *quiet {
		fmt.Fprintf(stdout, "%s %s\n", time.Now().Format(time.RFC3339), strings.Join(summary, " "))
	}
	if failed {
		return 1
	}
	return 0
}

func run(ctx context.Context, p reddit.Provider, t reddit.Target, limit int) ([]reddit.Post, *reddit.Error) {
	if t.Kind == reddit.TargetPost {
		post, err := p.Post(ctx, t.PostID, t.Subreddit)
		if err != nil {
			return nil, reddit.AsError(err)
		}
		return []reddit.Post{*post}, nil
	}
	page, err := p.List(ctx, reddit.Query{Target: t, Limit: limit})
	if err != nil {
		return nil, reddit.AsError(err)
	}
	return page.Posts, nil
}

// checkMedia fetches the first image and the first video manifest; ok is
// false when one of them can't be loaded.
func checkMedia(ctx context.Context, c *browser.Client, posts []reddit.Post) (summary string, ok bool) {
	var out []string
	img, vid, ok := false, false, true
	check := func(what string, r *browser.Response, err error) {
		if err != nil || r.Status != 200 {
			ok = false
		}
		out = append(out, what+" "+result(r, err))
	}
	for _, p := range posts {
		switch {
		case !img && (p.Kind == reddit.KindImage || p.Kind == reddit.KindGallery) && len(p.Media) > 0:
			img = true
			r, err := c.Download(ctx, p.Media[0].URL, "https://www.reddit.com/", io.Discard, 50<<20)
			check("image", r, err)
		case !vid && p.Kind == reddit.KindVideo && p.VideoID != "":
			vid = true
			r, err := c.Download(ctx, reddit.ManifestURL(p.VideoID), "https://www.reddit.com/", io.Discard, 5<<20)
			check("video manifest", r, err)
		}
	}
	if len(out) == 0 {
		return "", ok
	}
	return ", " + strings.Join(out, ", "), ok
}

func result(r *browser.Response, err error) string {
	if err != nil {
		if errors.Is(err, browser.ErrNotAllowed) {
			return "not allowed"
		}
		return "error"
	}
	return fmt.Sprintf("HTTP %d", r.Status)
}

func report(w io.Writer, name, client string, resps []response, posts []reddit.Post, err *reddit.Error, media string) {
	fmt.Fprintf(w, "== %s (client: %s)\n", name, client)
	for _, r := range resps {
		lower := strings.ToLower(string(r.body))
		var marks []string
		for _, m := range []struct {
			label string
			texts []string
		}{
			{"JS challenge", native.Markers.Challenge}, {"block page", native.Markers.Blocked}, {"rate limit", native.Markers.RateLimit},
			{"age gate", native.Markers.AgeGate},
			{"quarantine", native.Markers.Quarantine}, {"private", native.Markers.Private}, {"banned", native.Markers.Banned},
			{"not found", native.Markers.NotFound},
		} {
			label := m.label
			for _, t := range m.texts {
				if strings.Contains(lower, t) {
					marks = append(marks, label)
					break
				}
			}
		}
		if strings.Contains(r.url, "/login") {
			marks = append(marks, "login page")
		}
		posts := len(postTags.FindAllString(lower, -1))
		if n := len(nsfwPosts.FindAllString(lower, -1)); n > 0 {
			marks = append(marks, fmt.Sprintf("%d NSFW posts", n))
		}
		fmt.Fprintf(w, "  HTTP %d  %7d bytes  %3d <%s>  %s %s\n", r.status, len(r.body), posts, native.Layout.PostTag, r.url,
			strings.Join(marks, ", "))
	}
	if err != nil {
		fmt.Fprintf(w, "  FAILED: %s: %s\n", err.Class, err.Reason)
		if err.Hint != "" {
			fmt.Fprintf(w, "  hint: %s\n", err.Hint)
		}
		if err.Snippet != "" {
			fmt.Fprintf(w, "  page starts with: %.300q\n", err.Snippet)
		}
		return
	}
	cover := map[string]int{}
	for _, p := range posts {
		for field, ok := range map[string]bool{"title": p.Title != "", "author": p.Author != "", "created": !p.Created.IsZero(),
			"kind": p.Kind != "", "media key": p.MediaKey() != "", "thumbnail": p.Thumbnail != "", "subreddit": p.Subreddit != ""} {
			if ok {
				cover[field]++
			}
		}
	}
	fmt.Fprintf(w, "  %d posts; fields found: ", len(posts))
	for _, f := range []string{"title", "author", "subreddit", "created", "kind", "media key", "thumbnail"} {
		fmt.Fprintf(w, "%s %d/%d  ", f, cover[f], len(posts))
	}
	fmt.Fprintln(w)
	for i, p := range posts {
		if i == 8 {
			fmt.Fprintf(w, "  …\n")
			break
		}
		fmt.Fprintf(w, "  %-8s %-8s %-28s %s  %.50q\n", p.ID, p.Kind, p.MediaKey(), p.Created.Format("2006-01-02 15:04"), p.Title)
	}
	if media != "" {
		fmt.Fprintf(w, "  media:%s\n", strings.TrimPrefix(media, ","))
	}
}

var (
	// postTags matches the post elements only (not shreddit-post-flair).
	postTags = regexp.MustCompile(`<` + native.Layout.PostTag + `[\s>]`)
	// nsfwPosts matches post elements with an NSFW attribute; the word
	// alone is in the scripts and styles of every page.
	nsfwPosts = regexp.MustCompile(`<` + native.Layout.PostTag + `\s[^>]*\s(?:` + strings.Join(native.Layout.NSFW, "|") + `)(?:[\s>]|="(?:|true)")`)
	// Attributes with user names, titles, captions and labels (which
	// repeat the title).
	htmlAttrs = regexp.MustCompile(`(\s(?:author|author-name|author-avatar-url|post-title|alt|aria-label|arialabel)=")[^"]*"`)
	jsonAttrs = regexp.MustCompile(`("(?:author|title|selftext|selftext_html|author_fullname|author_flair_text|body|body_html|caption)"\s*:\s*")(?:[^"\\]|\\.)*"`)
	// The title is also in every permalink, as its last part, in the
	// slug parameter of partials, and at the start of preview names.
	slugs        = regexp.MustCompile(`((?:/|\\/)comments(?:/|\\/)[A-Za-z0-9]+(?:/|\\/))[^/"'\\?#&<>\s]+`)
	slugParams   = regexp.MustCompile(`((?:[?&;]|%26|%3F)slug(?:=|%3D))[^&"'%\s]+`)
	previewSlugs = regexp.MustCompile(`((?:preview|external-preview|i)\.redd\.it(?:/|\\/))[^/"'\\?#&<>\s]+?(-v\d+-)`)
	userLinks    = regexp.MustCompile(`((?:/|\\/)(?:user|u)(?:/|\\/))[A-Za-z0-9_-]+`)
	// The text of the page title, of a post's title element, and the
	// comments.
	texts        = regexp.MustCompile(`(<title>|<[a-z0-9-]+\s[^>]*slot="title"[^>]*>)[^<]*`)
	titleAttrs   = regexp.MustCompile(`(<shreddit-title\s[^>]*title=")[^"]*"`)
	commentTrees = regexp.MustCompile(`(?s)(<shreddit-comment-tree(?:\s[^>]*)?>).*?(</shreddit-comment-tree>)`)
	// What is replaced wherever else it appears: user names and titles.
	namesHTML = regexp.MustCompile(`\s(?:author|author-name|post-title)="([^"]+)"`)
	namesJSON = regexp.MustCompile(`"(?:author|title)"\s*:\s*"((?:[^"\\]|\\.)+)"`)
)

// anonymize replaces user names, titles and texts; structure and IDs stay.
// The titles Reddit gives removed posts stay too, as the parser needs them.
func anonymize(b []byte) []byte {
	words := personal(b)
	b = commentTrees.ReplaceAll(b, []byte(`${1}${2}`))
	b = texts.ReplaceAllFunc(b, func(m []byte) []byte {
		sm := texts.FindSubmatch(m)
		if native.RemovedTitle(string(m[len(sm[1]):])) {
			return m
		}
		return append(sm[1], 'x')
	})
	b = titleAttrs.ReplaceAll(b, []byte(`${1}x"`))
	b = htmlAttrs.ReplaceAllFunc(b, keepRemoved(htmlAttrs))
	b = slugs.ReplaceAll(b, []byte(`${1}x`))
	b = slugParams.ReplaceAll(b, []byte(`${1}x`))
	b = previewSlugs.ReplaceAll(b, []byte(`${1}x${2}`))
	b = userLinks.ReplaceAll(b, []byte(`${1}x`))
	b = jsonAttrs.ReplaceAllFunc(b, keepRemoved(jsonAttrs))
	for _, w := range words {
		b = replaceWord(b, w)
	}
	return b
}

// keepRemoved replaces an attribute's value with x unless it is the title
// of a removed post.
func keepRemoved(re *regexp.Regexp) func([]byte) []byte {
	return func(m []byte) []byte {
		prefix := re.FindSubmatch(m)[1]
		if native.RemovedTitle(strings.TrimSuffix(string(m[len(prefix):]), `"`)) {
			return m
		}
		return append(append([]byte(nil), prefix...), 'x', '"')
	}
}

// personal returns the user names and titles of a page that are long
// enough to be replaced as words, longest first.
func personal(b []byte) [][]byte {
	seen := map[string]bool{}
	var out [][]byte
	for _, re := range []*regexp.Regexp{namesHTML, namesJSON} {
		for _, m := range re.FindAllSubmatch(b, -1) {
			v := string(m[1])
			if len(v) < 4 || seen[v] || native.RemovedTitle(v) || v == "AutoModerator" {
				continue
			}
			seen[v] = true
			out = append(out, m[1])
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// replaceWord replaces w with x where it is not part of a longer name.
func replaceWord(b, w []byte) []byte {
	isName := func(c byte) bool {
		return c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	var out []byte
	for {
		i := bytes.Index(b, w)
		if i < 0 {
			return append(out, b...)
		}
		end := i + len(w)
		if (i > 0 && isName(b[i-1]) && isName(w[0])) || (end < len(b) && isName(b[end]) && isName(w[len(w)-1])) {
			out = append(out, b[:end]...)
		} else {
			out = append(append(out, b[:i]...), 'x')
		}
		b = b[end:]
	}
}

func save(dir, provider string, resps []response) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i, r := range resps {
		ext := ".html"
		if strings.HasPrefix(strings.TrimSpace(string(r.body)), "{") {
			ext = ".json"
		}
		name := filepath.Join(dir, fmt.Sprintf("%s-%02d%s", provider, i+1, ext))
		if err := os.WriteFile(name, anonymize(r.body), 0o644); err != nil {
			return err
		}
	}
	return nil
}
