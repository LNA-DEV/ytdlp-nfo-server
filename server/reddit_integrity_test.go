package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"image/png"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/manager"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/reddit/browser"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/fakereddit"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/scenario"
)

// The Reddit integrity tests run the test binary itself as the server, so
// it can reach the fake Reddit of the parent test through these variables;
// the production binary has no such hook.
const (
	envRedditAddr = "INTEGRITY_REDDIT_ADDR"
	envRedditCA   = "INTEGRITY_REDDIT_CA"
)

func redditServer() int {
	pool := x509.NewCertPool()
	pem, err := os.ReadFile(os.Getenv(envRedditCA))
	if err != nil || !pool.AppendCertsFromPEM(pem) {
		log.Printf("reddit test CA: %v", err)
		return 1
	}
	addr := os.Getenv(envRedditAddr)
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	if err := run(func(o *manager.Options) { o.Reddit = &browser.Options{RootCAs: pool, Dial: dial} }); err != nil {
		log.Print(err)
		return 1
	}
	return 0
}

const (
	galleryPost   = "intgal"
	galleryFolder = "Gallery Test [intgal]"
)

var galleryMedia = []string{"gm1", "gm2", "gm3"}

func galleryKey() string {
	media := make([]reddit.Media, len(galleryMedia))
	for i, id := range galleryMedia {
		media[i] = reddit.Media{MediaID: id}
	}
	return "redditgallery " + reddit.GalleryHash(media)
}

type redditCase struct {
	dir, data, dl, media string
	site                 *fakereddit.Site
	env                  []string
}

func newRedditCase(t *testing.T, fake string) *redditCase {
	c := &redditCase{dir: t.TempDir()}
	c.data, c.dl, c.media = filepath.Join(c.dir, "data"), filepath.Join(c.dir, "downloads"), filepath.Join(c.dir, "media")
	c.site = fakereddit.NewSite(t)
	c.site.Add(&fakereddit.Post{ID: galleryPost, Subreddit: "pics", Title: "Gallery Test", Kind: "gallery", Media: galleryMedia,
		Created: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)})
	sc := &scenario.Scenario{Version: "2026.01.01", Targets: []string{"Chrome-131 Windows-10"}}
	scPath := filepath.Join(c.dir, "scenario.json")
	sc.Write(scPath)
	state := filepath.Join(c.dir, "state")
	os.MkdirAll(state, 0o755)
	ca := filepath.Join(c.dir, "ca.pem")
	os.WriteFile(ca, c.site.CAPEM, 0o644)
	c.env = append(serverEnv(c.dir, fake, scPath, state), envRedditAddr+"="+c.site.Addr(), envRedditCA+"="+ca)
	return c
}

func (c *redditCase) start(t *testing.T, failpoint string) *serverProc {
	p := startServer(t, os.Args[0], c.env, failpoint)
	st := p.call(t, "GET", "/api/v1/settings", nil)["settings"].(map[string]any)
	st["minFreeSpaceMB"] = 0
	p.call(t, "PUT", "/api/v1/settings", st)
	return p
}

func (c *redditCase) submit(t *testing.T, p *serverProc, id string, force bool) string {
	job := p.call(t, "POST", "/api/v1/jobs", map[string]any{"url": "https://www.reddit.com/r/pics/comments/" + id + "/x/",
		"options": map[string]any{"force": force}})
	if job == nil || job["id"] == nil {
		t.Fatalf("submit failed: %v\n%s", job, p.out)
	}
	return job["id"].(string)
}

func waitKilled(t *testing.T, p *serverProc, fp string) {
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("failpoint %s not reached\n%s", fp, p.out)
	}
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("server was not killed at %s: %v\n%s", fp, p.cmd.ProcessState, p.out)
	}
}

func waitCompleted(t *testing.T, p *serverProc, id string) {
	deadline := time.Now().Add(20 * time.Second)
	for {
		j := p.call(t, "GET", "/api/v1/jobs/"+id, nil)
		if j != nil && j["state"] == "completed" {
			return
		}
		if j != nil && (j["state"] == "failed" || j["state"] == "partial") {
			t.Fatalf("job failed: %v\n%s", j, p.out)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not complete: %v\n%s", j, p.out)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// checkGallery asserts invariants 1 and 2 for the gallery folder: absent,
// the old version, or complete (marker and every image decodes). It
// returns "", "old" or "new".
func checkGallery(t *testing.T, root string) string {
	t.Helper()
	entries, _ := os.ReadDir(root)
	version := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.Name() != galleryFolder {
			t.Fatalf("unexpected library entry %q", e.Name())
		}
		dir := filepath.Join(root, e.Name())
		m := library.ReadMarker(dir)
		if m == nil || m.Key != galleryKey() {
			t.Fatalf("gallery folder without a complete marker: %+v", m)
		}
		if old, err := os.ReadFile(filepath.Join(dir, "old.png")); err == nil && string(old) == "OLD" {
			version = "old"
			continue
		}
		if len(m.Files) != len(galleryMedia) {
			t.Fatalf("marker lists %d files", len(m.Files))
		}
		for i := range galleryMedia {
			name := fmt.Sprintf("%s - %02d.png", galleryFolder, i+1)
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("incomplete gallery in the library: %v", err)
			}
			if _, err := png.Decode(bytes.NewReader(data)); err != nil {
				t.Fatalf("broken image in the library: %s: %v", name, err)
			}
		}
		version = "new"
	}
	return version
}

func mediaRows(t *testing.T, data, key string) (archived bool, rows int) {
	t.Helper()
	st, err := store.Open(filepath.Join(data, "ytdlp-nfo-server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	archived, _ = st.HasArchive(context.Background(), key)
	r, _ := st.MediaOf(context.Background(), key)
	return archived, len(r)
}

// TestIntegrityRedditKill kills the server at every step of a gallery
// download, restarts it and checks the invariants of §4.5 together with
// the media index (Reddit concept §13): verified images are not
// downloaded again, and the media index rows exist if and only if the
// archive entry does.
func TestIntegrityRedditKill(t *testing.T) {
	if testing.Short() {
		t.Skip("integrity test runs the server")
	}
	_, fake := buildServer(t)
	points := []string{
		"pipeline:probed", "reddit:image-saved", "pipeline:verified",
		"finalize:before-marker", "finalize:staged", "finalize:swapped", "pipeline:recorded",
	}
	for _, replace := range []bool{false, true} {
		for _, fp := range points {
			name := fp
			if replace {
				name += "/replace-existing"
			}
			t.Run(name, func(t *testing.T) { runRedditCrash(t, fake, fp, replace) })
		}
	}
}

func runRedditCrash(t *testing.T, fake, fp string, replace bool) {
	c := newRedditCase(t, fake)
	if replace {
		old := filepath.Join(c.media, galleryFolder)
		os.MkdirAll(old, 0o755)
		os.WriteFile(filepath.Join(old, "old.png"), []byte("OLD"), 0o644)
		m := &library.Marker{Key: galleryKey(), Post: galleryPost, Kind: "gallery"}
		b, _ := m.Encode()
		os.WriteFile(filepath.Join(old, library.MarkerName), b, 0o644)
	}

	p := c.start(t, fp)
	id := c.submit(t, p, galleryPost, replace)
	waitKilled(t, p, fp)

	version := checkGallery(t, c.media)
	if replace && version == "" {
		t.Fatal("existing gallery folder vanished (invariant 3)")
	}
	archived, rows := mediaRows(t, c.data, galleryKey())
	if archived != (rows == len(galleryMedia)) || archived && version != "new" {
		t.Fatalf("archive entry %v, %d media rows, library %q (invariant 4)", archived, rows, version)
	}

	p = c.start(t, "")
	waitCompleted(t, p, id)
	p.stop(t)
	if v := checkGallery(t, c.media); v != "new" {
		t.Fatalf("library after resume: %q", v)
	}
	if archived, rows := mediaRows(t, c.data, galleryKey()); !archived || rows != len(galleryMedia) {
		t.Fatalf("after resume: archive %v, %d media rows", archived, rows)
	}
	if n := c.site.Count("i.redd.it", "/"); n != len(galleryMedia) {
		t.Fatalf("%d image downloads: verified images were downloaded again", n)
	}
	for _, leftover := range []string{filepath.Join(c.media, ".staging"), filepath.Join(c.media, ".trash"), filepath.Join(c.dl, ".work")} {
		if entries, _ := os.ReadDir(leftover); len(entries) > 0 {
			t.Fatalf("leftovers in %s", leftover)
		}
	}
	if arch, _ := os.ReadFile(filepath.Join(c.dl, ".ytdlp-archive.txt")); len(arch) != 0 {
		t.Fatalf("gallery key in the archive file: %q", arch)
	}
}

// TestIntegrityRedditDuplicateKill kills the server right before and
// right after a duplicate is recorded: the skipped item and its alias are
// both there or both missing (Reddit concept §13).
func TestIntegrityRedditDuplicateKill(t *testing.T) {
	if testing.Short() {
		t.Skip("integrity test runs the server")
	}
	_, fake := buildServer(t)
	for _, fp := range []string{"reddit:duplicate-found", "reddit:duplicate-recorded"} {
		t.Run(fp, func(t *testing.T) {
			c := newRedditCase(t, fake)
			c.site.SetImage("dupimg", fakereddit.MediaBytes(galleryMedia[0], false))
			c.site.Add(&fakereddit.Post{ID: "intdup", Subreddit: "pics", Title: "Repost", Kind: "image", Media: []string{"dupimg"},
				Created: time.Now()})
			p := c.start(t, fp)
			waitCompleted(t, p, c.submit(t, p, galleryPost, false))
			id := c.submit(t, p, "intdup", false)
			waitKilled(t, p, fp)

			check := func(final bool) {
				st, err := store.Open(filepath.Join(c.data, "ytdlp-nfo-server.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				jobID, _ := strconv.ParseInt(id, 10, 64)
				its, _ := st.ItemsForJob(context.Background(), jobID)
				alias, _ := st.MediaByKey(context.Background(), "redditmedia dupimg")
				skipped := len(its) == 1 && its[0].State == store.ItemSkipped
				if skipped != (alias != nil) || final && !skipped {
					t.Fatalf("skipped %v, alias %+v", skipped, alias)
				}
				if alias != nil && (!alias.Alias || alias.Path != galleryFolder+"/"+galleryFolder+" - 01.png") {
					t.Fatalf("alias: %+v", alias)
				}
			}
			check(false)
			p = c.start(t, "")
			waitCompleted(t, p, id)
			p.stop(t)
			check(true)
			if v := checkGallery(t, c.media); v != "new" {
				t.Fatalf("library: %q", v)
			}
		})
	}
}
