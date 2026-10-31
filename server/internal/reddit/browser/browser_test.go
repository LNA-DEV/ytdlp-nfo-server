package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/fakereddit"
)

func newClient(t *testing.T, fake *fakereddit.Server, jar string) *Client {
	t.Helper()
	c, err := New(Options{JarPath: jar, RootCAs: fake.Roots, Dial: fake.Dial})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestDocumentHeadersAndCookies(t *testing.T) {
	fake := fakereddit.New(t)
	fake.Handle("www.reddit.com", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/r/pics/new/" {
			http.SetCookie(w, &http.Cookie{Name: "loid", Value: "abc", Path: "/", MaxAge: 3600})
		}
		fmt.Fprint(w, "<html>ok</html>")
	})
	jar := filepath.Join(t.TempDir(), "cookies.json")
	c := newClient(t, fake, jar)
	ctx := context.Background()
	resp, err := c.Document(ctx, "https://www.reddit.com/r/pics/new/", "", 1<<20)
	if err != nil || resp.Status != 200 || string(resp.Body) != "<html>ok</html>" {
		t.Fatalf("document: %v %+v", err, resp)
	}
	if _, err := c.Partial(ctx, "https://www.reddit.com/svc/shreddit/more", "https://www.reddit.com/r/pics/new/", 1<<20); err != nil {
		t.Fatal(err)
	}
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests: %d", len(reqs))
	}
	doc, part := reqs[0].Header, reqs[1].Header
	if doc.Get("Sec-Fetch-Mode") != "navigate" || doc.Get("Sec-Fetch-Site") != "none" || doc.Get("Sec-Fetch-Dest") != "document" ||
		doc.Get("User-Agent") != Chrome.UserAgent || doc.Get("Sec-Ch-Ua") != Chrome.SecChUa || doc.Get("Referer") != "" {
		t.Fatalf("document headers: %v", doc)
	}
	if !strings.Contains(doc.Get("Cookie"), "over18=1") {
		t.Fatalf("over18 cookie missing: %q", doc.Get("Cookie"))
	}
	if part.Get("Sec-Fetch-Mode") != "cors" || part.Get("Sec-Fetch-Site") != "same-origin" ||
		part.Get("Referer") != "https://www.reddit.com/r/pics/new/" || !strings.Contains(part.Get("Cookie"), "loid=abc") {
		t.Fatalf("partial headers: %v", part)
	}
	// The jar survives a restart and is reset after a block.
	c2 := newClient(t, fake, jar)
	if c2.Jar().Len() != 1 {
		t.Fatalf("persisted cookies: %d", c2.Jar().Len())
	}
	c2.Jar().Reset()
	if newClient(t, fake, jar).Jar().Len() != 0 {
		t.Fatal("reset not persisted")
	}
}

// Chrome sends its headers in a fixed order; the order is part of the
// fingerprint (§5.1).
func TestHeaderOrder(t *testing.T) {
	c, _ := New(Options{})
	var names []string
	for _, h := range c.headers(kindDocument, mustURL("https://www.reddit.com/r/x/new/"), "") {
		names = append(names, h[0])
	}
	want := "sec-ch-ua sec-ch-ua-mobile sec-ch-ua-platform upgrade-insecure-requests user-agent accept sec-fetch-site " +
		"sec-fetch-mode sec-fetch-user sec-fetch-dest accept-encoding accept-language priority"
	if strings.Join(names, " ") != want {
		t.Fatalf("order:\n%s\nwant\n%s", strings.Join(names, " "), want)
	}
}

func TestAllowlistAndRedirects(t *testing.T) {
	fake := fakereddit.New(t)
	fake.Handle("www.reddit.com", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, "https://example.com/lan", http.StatusFound)
		case "/media":
			http.Redirect(w, r, "https://i.redd.it/abc.jpg", http.StatusFound)
		}
	})
	fake.Handle("i.redd.it", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("\xff\xd8\xffjpeg")) })
	c := newClient(t, fake, "")
	ctx := context.Background()
	for _, u := range []string{"https://example.com/x", "http://www.reddit.com/", "https://www.reddit.com:8443/"} {
		if _, err := c.Document(ctx, u, "", 1<<20); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if _, err := c.Document(ctx, "https://www.reddit.com/away", "", 1<<20); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("redirect off the allowlist: %v", err)
	}
	var buf bytes.Buffer
	resp, err := c.Download(ctx, "https://www.reddit.com/media", "https://www.reddit.com/", &buf, 1<<20)
	if err != nil || resp.Status != 200 || buf.String() != "\xff\xd8\xffjpeg" {
		t.Fatalf("download: %v %+v %q", err, resp, buf.String())
	}
	buf.Reset()
	if _, err := c.Download(ctx, "https://i.redd.it/abc.jpg", "", &buf, 3); err == nil {
		t.Fatal("size limit not enforced")
	}
}

func TestUntrustedCertificate(t *testing.T) {
	fake := fakereddit.New(t)
	other := fakereddit.New(t)
	c, err := New(Options{RootCAs: other.Roots, Dial: fake.Dial})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Document(context.Background(), "https://www.reddit.com/", "", 1<<20); err == nil {
		t.Fatal("a certificate of another CA was accepted")
	}
}

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2a00:1450::1": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false, "169.254.1.1": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fd00::1": false, "fe80::1": false, "::ffff:192.168.1.1": false,
	} {
		if got := PublicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v", addr, got)
		}
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
