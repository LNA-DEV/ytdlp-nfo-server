package versions

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

func fakeScript(version string, smokeFails bool) string {
	exit := "0"
	if smokeFails {
		exit = "3"
	}
	return fmt.Sprintf(`#!/bin/sh
case "$*" in
  *--version*) echo %s; exit %s;;
  *--list-impersonate-targets*) printf '[info] Available impersonate targets\nClient OS Source\n------\nChrome-131 Windows-10 curl_cffi\nSafari Macos-14 curl_cffi (unavailable)\n'; exit 0;;
esac
exit 0
`, version, exit)
}

func zipWith(_ *testing.T, exe string, script string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: exe, Method: zip.Deflate}
	h.SetMode(0o755)
	w, _ := zw.CreateHeader(h)
	w.Write([]byte(script))
	h2 := &zip.FileHeader{Name: "_internal/lib.so"}
	h2.SetMode(0o644)
	w2, _ := zw.CreateHeader(h2)
	w2.Write([]byte("lib"))
	zw.Close()
	return buf.Bytes()
}

type fakeGitHub struct {
	srv     *httptest.Server
	assets  map[string][]byte // path -> body
	apiFail bool
	latest  map[string]string
	mu      sync.Mutex
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{assets: map[string][]byte{}, latest: map[string]string{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/releases") {
			if g.apiFail {
				http.Error(w, "rate limited", http.StatusForbidden)
				return
			}
			repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/releases")
			var tags []map[string]any
			for p := range g.assets {
				if strings.HasPrefix(p, "/"+repo+"/releases/download/") && strings.HasSuffix(p, "/SHA2-256SUMS") {
					tag := strings.Split(strings.TrimPrefix(p, "/"+repo+"/releases/download/"), "/")[0]
					tags = append(tags, map[string]any{"tag_name": tag, "published_at": "2026-10-01T00:00:00Z"})
				}
			}
			json.NewEncoder(w).Encode(tags)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/releases/latest")
			w.Header().Set("Location", "/"+repo+"/releases/tag/"+g.latest[repo])
			w.WriteHeader(http.StatusFound)
			return
		}
		if b, ok := g.assets[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) publish(channel, tag string, zipBody []byte, corruptSum bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	repo := Repos[channel]
	base := "/" + repo + "/releases/download/" + tag + "/"
	sum := sha256.Sum256(zipBody)
	hexsum := hex.EncodeToString(sum[:])
	if corruptSum {
		hexsum = strings.Repeat("0", 64)
	}
	g.assets[base+"yt-dlp_linux.zip"] = zipBody
	g.assets[base+"SHA2-256SUMS"] = []byte(hexsum + "  yt-dlp_linux.zip\n" + strings.Repeat("1", 64) + "  yt-dlp.exe\n")
	g.latest[repo] = tag
}

type memSettings struct {
	mu sync.Mutex
	s  config.Settings
}

func (m *memSettings) Settings() config.Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s
}

func (m *memSettings) UpdateYtdlp(ctx context.Context, fn func(*config.YtdlpSettings)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.s.Ytdlp)
	return nil
}

func newManager(t *testing.T, g *fakeGitHub) (*Manager, *memSettings) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ms := &memSettings{s: config.DefaultSettings(false)}
	m := &Manager{
		Store: st, Settings: ms, Arch: "amd64", Dir: filepath.Join(t.TempDir(), "ytdlp"),
		Fetcher: &Fetcher{DownloadURL: g.srv.URL}, APIURL: g.srv.URL, Logf: t.Logf,
	}
	return m, ms
}

func TestInstallActivateAndAcquire(t *testing.T) {
	g := newFakeGitHub(t)
	g.publish("nightly", "2026.10.01.1", zipWith(t, "yt-dlp_linux", fakeScript("2026.10.01.1", false)), false)
	m, ms := newManager(t, g)
	ctx := context.Background()

	rels, err := m.Releases(ctx, "nightly")
	if err != nil || len(rels) != 1 || rels[0].Tag != "2026.10.01.1" {
		t.Fatalf("releases = %v, %v", rels, err)
	}
	v, err := m.Install(ctx, "nightly", "2026.10.01.1")
	if err != nil {
		t.Fatal(err)
	}
	if !v.SmokeOK || v.Version != "2026.10.01.1" || len(v.ImpersonateTargets) != 1 || v.ImpersonateTargets[0] != "chrome-131:windows-10" {
		t.Fatalf("version = %+v", v)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(v.Path), "_internal", "lib.so")); err != nil {
		t.Fatal("onedir payload missing")
	}
	if err := m.Activate(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if ms.Settings().Ytdlp.Active != "nightly/2026.10.01.1" {
		t.Fatal("not active")
	}
	b, release, err := m.Acquire(ctx, "")
	if err != nil || b.Path != v.Path || !b.ImpersonateAvailable {
		t.Fatalf("acquire = %+v %v", b, err)
	}
	if err := m.Delete(ctx, v.ID); err == nil {
		t.Fatal("deleted active version")
	}
	release()
}

func TestChecksumMismatchRejected(t *testing.T) {
	g := newFakeGitHub(t)
	g.publish("stable", "2026.09.01", zipWith(t, "yt-dlp_linux", fakeScript("2026.09.01", false)), true)
	m, _ := newManager(t, g)
	_, err := m.Install(context.Background(), "stable", "2026.09.01")
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "stable", "2026.09.01")); err == nil {
		t.Fatal("unverified build left on disk")
	}
}

func TestFailedSmokeBlocksActivation(t *testing.T) {
	g := newFakeGitHub(t)
	g.publish("master", "2026.10.02.5", zipWith(t, "yt-dlp_linux", fakeScript("x", true)), false)
	m, _ := newManager(t, g)
	ctx := context.Background()
	v, err := m.Install(ctx, "master", "2026.10.02.5")
	if err != nil {
		t.Fatal(err)
	}
	if v.SmokeOK {
		t.Fatal("smoke test should fail")
	}
	if err := m.Activate(ctx, v.ID); err == nil {
		t.Fatal("activated a version that failed its smoke test")
	}
}

func TestUnsafeZipRejected(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../evil")
	w.Write([]byte("x"))
	zw.Close()
	g := newFakeGitHub(t)
	g.publish("stable", "2026.09.02", buf.Bytes(), false)
	m, _ := newManager(t, g)
	if _, err := m.Install(context.Background(), "stable", "2026.09.02"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestLatestFallbackAndAutoUpdate(t *testing.T) {
	g := newFakeGitHub(t)
	g.publish("nightly", "2026.10.01.1", zipWith(t, "yt-dlp_linux", fakeScript("2026.10.01.1", false)), false)
	m, ms := newManager(t, g)
	ctx := context.Background()
	v, err := m.Install(ctx, "nightly", "2026.10.01.1")
	if err != nil {
		t.Fatal(err)
	}
	m.Activate(ctx, v.ID)

	g.apiFail = true // rate limited: fall back to releases/latest redirect
	g.publish("nightly", "2026.10.05.1", zipWith(t, "yt-dlp_linux", fakeScript("2026.10.05.1", false)), false)
	m.cache = map[string]cachedReleases{}
	m.CheckUpdates(ctx)
	if got := ms.Settings().Ytdlp.Active; got != "nightly/2026.10.05.1" {
		t.Fatalf("active after auto-update = %q", got)
	}
}

func TestRetentionKeepsActivePinnedAndNewest(t *testing.T) {
	g := newFakeGitHub(t)
	m, _ := newManager(t, g)
	ctx := context.Background()
	install := func(tag string) {
		g.publish("stable", tag, zipWith(t, "yt-dlp_linux", fakeScript(tag, false)), false)
		if _, err := m.Install(ctx, "stable", tag); err != nil {
			t.Fatal(err)
		}
	}
	for _, tag := range []string{"2026.01.01", "2026.02.01", "2026.03.01"} {
		install(tag)
	}
	m.Activate(ctx, "stable/2026.01.01")
	m.SetPinned(ctx, "stable/2026.02.01", true)
	_, release, _ := m.Acquire(ctx, "stable/2026.03.01")
	for _, tag := range []string{"2026.04.01", "2026.05.01", "2026.06.01"} {
		install(tag) // each install runs retention
	}
	release()
	all, _ := m.Store.ListVersions(ctx)
	var got []string
	for _, v := range all {
		got = append(got, v.Tag)
	}
	want := "2026.06.01,2026.05.01,2026.04.01,2026.03.01,2026.02.01,2026.01.01"
	if strings.Join(got, ",") != want {
		// 03.01 in use, 02.01 pinned, 01.01 active: nothing may be removed
		t.Fatalf("kept %v", got)
	}
	m.GC(ctx) // 03.01 no longer in use -> removed
	all, _ = m.Store.ListVersions(ctx)
	if len(all) != 5 {
		t.Fatalf("expected 5 after GC, got %d", len(all))
	}
}

func TestCompareTags(t *testing.T) {
	if CompareTags("2026.10.10.233141", "2026.10.10") <= 0 || CompareTags("2026.9.1", "2026.10.1") >= 0 || CompareTags("2026.01.01", "2026.01.01") != 0 {
		t.Fatal("tag ordering wrong")
	}
}
