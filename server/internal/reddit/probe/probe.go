// Package probe is the `reddit-probe` diagnostics command (Reddit concept
// §13): it runs the providers from the server's network and shows what
// Reddit and Arctic Shift really answer, optionally saving the pages as
// anonymized test fixtures.
package probe

import (
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
	"strings"
	"sync"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/arcticshift"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
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
	arc := arcticshift.New(c)
	if testDial != nil {
		nat.PageDelay, arc.MinInterval = nil, 0
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
		media := ""
		if perr == nil {
			media = checkMedia(ctx, c, posts)
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

// checkMedia fetches the first image and the first video manifest.
func checkMedia(ctx context.Context, c *browser.Client, posts []reddit.Post) string {
	var out []string
	img, vid := false, false
	for _, p := range posts {
		switch {
		case !img && (p.Kind == reddit.KindImage || p.Kind == reddit.KindGallery) && len(p.Media) > 0:
			img = true
			r, err := c.Download(ctx, p.Media[0].URL, "https://www.reddit.com/", io.Discard, 50<<20)
			out = append(out, "image "+result(r, err))
		case !vid && p.Kind == reddit.KindVideo && p.VideoID != "":
			vid = true
			r, err := c.Download(ctx, reddit.ManifestURL(p.VideoID), "https://www.reddit.com/", io.Discard, 5<<20)
			out = append(out, "video manifest "+result(r, err))
		}
	}
	if len(out) == 0 {
		return ""
	}
	return ", " + strings.Join(out, ", ")
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
			{"block page", native.Markers.Blocked}, {"rate limit", native.Markers.RateLimit}, {"age gate", native.Markers.AgeGate},
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
		if strings.Contains(lower, "blurred") || strings.Contains(lower, "nsfw") {
			marks = append(marks, "NSFW markup")
		}
		posts := strings.Count(lower, "<"+native.Layout.PostTag)
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
	htmlAttrs = regexp.MustCompile(`((?:author|post-title)=")[^"]*"`)
	jsonAttrs = regexp.MustCompile(`("(?:author|title|selftext|author_fullname)"\s*:\s*")(?:[^"\\]|\\.)*"`)
)

// anonymize replaces user names, titles and texts; structure and IDs stay.
func anonymize(b []byte) []byte {
	b = htmlAttrs.ReplaceAll(b, []byte(`${1}x"`))
	return jsonAttrs.ReplaceAll(b, []byte(`${1}x"`))
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
