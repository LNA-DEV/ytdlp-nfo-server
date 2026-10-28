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
		Media: []string{"img1"}, Created: now},
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
		if bytes.Contains(b, []byte("secretuser")) || bytes.Contains(b, []byte("Secret title")) {
			t.Fatalf("%s is not anonymized", f)
		}
	}
	site.SetNativeBlocked(true)
	out.Reset()
	code = Main([]string{"--quiet", "--provider", "native", "https://www.reddit.com/r/pics/"}, &out)
	if code != 1 || !strings.Contains(out.String(), "native=blocked(") {
		t.Fatalf("blocked: %d %s", code, out.String())
	}
}
