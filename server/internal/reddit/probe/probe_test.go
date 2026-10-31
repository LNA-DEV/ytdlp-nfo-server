package probe

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/fakereddit"
)

func TestProbe(t *testing.T) {
	site := fakereddit.NewSite(t)
	testRoots, testDial = site.Roots, site.Dial
	defer func() { testRoots, testDial = nil, nil }()
	now := time.Now()
	site.Add(&fakereddit.Post{ID: "p1", Subreddit: "pics", Author: "secretuser", Title: "Secret title", Kind: "image",
		Media: []string{"img1"}, Created: now, Slug: "secret_title"},
		&fakereddit.Post{ID: "p2", Subreddit: "pics", Author: "u2", Title: "Clip", Kind: "video", Media: []string{"vid2"},
			Created: now.Add(-time.Minute)})
	dir := t.TempDir()
	var out bytes.Buffer
	code := Main([]string{"--fixtures", dir, "https://www.reddit.com/r/pics/"}, &out)
	s := out.String()
	if code != 0 || !strings.Contains(s, "== native") || !strings.Contains(s, "== arcticshift") ||
		!strings.Contains(s, "2 posts; fields found: title 2/2") || !strings.Contains(s, "image HTTP 200") {
		t.Fatalf("exit %d:\n%s", code, s)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) < 2 {
		t.Fatalf("fixtures: %v", files)
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if bytes.Contains(b, []byte("secretuser")) || bytes.Contains(b, []byte("Secret title")) || bytes.Contains(b, []byte("secret_title")) {
			t.Fatalf("%s is not anonymized", f)
		}
	}
	// A post the providers list but whose image is gone fails the run.
	site.SetGone("img1")
	out.Reset()
	code = Main([]string{"--quiet", "--provider", "arcticshift", "https://www.reddit.com/r/pics/"}, &out)
	if code != 1 || !strings.Contains(out.String(), "arcticshift=media(2 posts, image HTTP 404") {
		t.Fatalf("gone image: %d %s", code, out.String())
	}
	site.SetNativeBlocked(true)
	out.Reset()
	code = Main([]string{"--quiet", "--provider", "native", "https://www.reddit.com/r/pics/"}, &out)
	if code != 1 || !strings.Contains(out.String(), "native=blocked(") {
		t.Fatalf("blocked: %d %s", code, out.String())
	}
}

func TestAnonymizeSlugs(t *testing.T) {
	in := `permalink="/r/example/comments/abc12/some_title_here/" {"permalink":"/r/example/comments/abc12/some_title_here/",` +
		`"url":"https:\/\/www.reddit.com\/r\/example\/comments\/abc12\/some_title_here\/?x=1"} /comments/abc12/`
	want := `permalink="/r/example/comments/abc12/x/" {"permalink":"/r/example/comments/abc12/x/",` +
		`"url":"https:\/\/www.reddit.com\/r\/example\/comments\/abc12\/x\/?x=1"} /comments/abc12/`
	if got := string(anonymize([]byte(in))); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// Names and titles go wherever they appear: links, labels, captions,
// preview names, partial parameters, text and comments. IDs and the
// titles of removed posts stay.
func TestAnonymizeTexts(t *testing.T) {
	in := `<title>My cat Whiskers : r/example</title><shreddit-title title="My cat Whiskers : r/example"></shreddit-title>` +
		`<shreddit-post id="t3_abc12" subreddit-prefixed-name="r/example" author="catlover_99" post-title="My cat Whiskers" author-avatar-url="https://styles.redditmedia.com/t5_x/avatar.png">` +
		`<h1 slot="title" aria-label="Post Title: My cat Whiskers">My cat Whiskers</h1>` +
		`<a href="/user/catlover_99/"><span>u/catlover_99</span></a>` +
		`<img alt="Whiskers asleep" src="https://preview.redd.it/my-cat-whiskers-v0-k3m9x2abc1yh1.jpeg?width=640&amp;s=1">` +
		`<faceplate-partial src="/svc/shreddit/partial/x?params=postId%3Dabc12%26slug%3Dmy_cat_whiskers&amp;sig=1"></faceplate-partial>` +
		`<shreddit-comment-tree post-id="t3_abc12"><shreddit-comment author="other_user">so cute!</shreddit-comment></shreddit-comment-tree>` +
		`</shreddit-post>` +
		`<shreddit-post id="t3_rem1" author="[deleted]" post-title="[ Removed by moderator ]"><h1 slot="title">[ Removed by moderator ]</h1></shreddit-post>` +
		`{"author":"catlover_99","author_flair_text":"Cat person","title":"My cat Whiskers","url":"https:\/\/preview.redd.it\/my-cat-v0-p7q2w8def1yh1.png"}`
	got := string(anonymize([]byte(in)))
	for _, s := range []string{"catlover", "Whiskers", "whiskers", "my-cat", "other_user", "so cute", "Cat person", "avatar.png"} {
		if strings.Contains(got, s) {
			t.Errorf("%q left in\n%s", s, got)
		}
	}
	for _, s := range []string{`id="t3_abc12"`, "x-v0-k3m9x2abc1yh1.jpeg", `x-v0-p7q2w8def1yh1.png`, "postId%3Dabc12%26slug%3Dx",
		`post-title="[ Removed by moderator ]"`, `<h1 slot="title">[ Removed by moderator ]</h1>`, "r/example", "</shreddit-comment-tree>"} {
		if !strings.Contains(got, s) {
			t.Errorf("%q lost\n%s", s, got)
		}
	}
}

// The report counts post elements only, and NSFW by the posts' attributes.
func TestReportCounts(t *testing.T) {
	page := `<html><style>.nsfw-blurred{}</style><shreddit-post id="t3_a1" nsfw><shreddit-post-flair></shreddit-post-flair></shreddit-post>` +
		`<shreddit-post id="t3_a2" post-title="x"></shreddit-post><shreddit-post id="t3_a3" nsfw="" subreddit-nsfw></shreddit-post>`
	var out bytes.Buffer
	report(&out, "native", "browser", []response{{url: "https://www.reddit.com/r/example/", status: 200, body: []byte(page)}}, nil, nil, "")
	if s := out.String(); !strings.Contains(s, "  3 <shreddit-post>") || !strings.Contains(s, "2 NSFW posts") || strings.Contains(s, "quarantine") {
		t.Fatalf("%s", s)
	}
}
