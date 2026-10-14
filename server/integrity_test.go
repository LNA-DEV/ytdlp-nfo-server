package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/scenario"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testutil.Cleanup() // the fake is built once per test binary and shared by all runs
	os.Exit(code)
}

// TestIntegrityKillAtEveryStep kills the real server binary with SIGKILL
// at every pipeline step, restarts it and checks the file-integrity
// invariants of §4.5:
//
//  1. nothing appears in the library before it passed verify (no partial
//     or incomplete item folders, ever);
//  2. items appear complete (single rename / atomic exchange);
//  3. existing library content is never removed before its replacement is
//     in place (force re-download variant);
//  4. an archive entry exists if and only if the item is in the library;
//  5. every step resumes after the crash.
func TestIntegrityKillAtEveryStep(t *testing.T) {
	if testing.Short() {
		t.Skip("integrity test builds and runs the server binary")
	}
	fake, err := testutil.FakeBin()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "ytdlp-nfo-server")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatal(err)
	}

	points := []string{
		"pipeline:probed", "pipeline:downloading", "pipeline:downloaded", "pipeline:verified", "pipeline:extras-done",
		"finalize:before-marker", "finalize:staged", "finalize:swapped", "pipeline:recorded",
	}
	for _, replace := range []bool{false, true} {
		for _, fp := range points {
			name := fp
			if replace {
				name += "/replace-existing"
			}
			t.Run(name, func(t *testing.T) { runCrashCase(t, bin, fake, fp, replace) })
		}
	}
}

type serverProc struct {
	cmd  *exec.Cmd
	port string
	done chan error
	out  *bytes.Buffer
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

func startServer(t *testing.T, bin string, env []string, failpoint string) *serverProc {
	t.Helper()
	p := &serverProc{port: freePort(t), done: make(chan error, 1), out: &bytes.Buffer{}}
	p.cmd = exec.Command(bin)
	p.cmd.Env = append(append(os.Environ(), env...), "PORT="+p.port, "YTDLP_NFO_FAILPOINTS="+failpoint)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get("http://127.0.0.1:" + p.port + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return p
			}
		}
		select {
		case err := <-p.done:
			t.Fatalf("server exited during start: %v\n%s", err, p.out)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start:\n%s", p.out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *serverProc) call(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://127.0.0.1:"+p.port+path, r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil // the server may just have been killed
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func (p *serverProc) stop(t *testing.T) {
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		p.cmd.Process.Kill()
		t.Fatalf("server did not stop:\n%s", p.out)
	}
}

const (
	videoID    = "intg"
	videoTitle = "Integrity Test"
	folderName = "Integrity Test [intg]"
)

// checkLibrary asserts invariants 1 and 2 and returns the media content of
// the item folder ("" if absent).
func checkLibrary(t *testing.T, root string) string {
	t.Helper()
	entries, _ := os.ReadDir(root)
	media := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.Name() != folderName {
			t.Fatalf("unexpected library entry %q", e.Name())
		}
		dir := filepath.Join(root, e.Name())
		files, _ := os.ReadDir(dir)
		// A ".ytdlp-nfo-staging" marker may be present right after a swap;
		// the content must be complete either way (checked below).
		names := map[string]bool{}
		for _, f := range files {
			names[f.Name()] = true
		}
		data, err := os.ReadFile(filepath.Join(dir, folderName+".mkv"))
		if err != nil {
			t.Fatalf("library folder without media: %v (%v)", err, names)
		}
		if string(data) != "OLD" {
			var info scenario.MediaInfo
			if json.Unmarshal(data, &info) != nil || !info.FakeMedia || info.Duration <= 0 {
				t.Fatalf("incomplete media in the library: %q", data)
			}
		}
		nfo, err := os.ReadFile(filepath.Join(dir, "movie.nfo"))
		if err != nil || !strings.HasSuffix(strings.TrimSpace(string(nfo)), "</movie>") {
			t.Fatalf("incomplete movie.nfo in the library: %v", err)
		}
		media = string(data)
	}
	return media
}

func runCrashCase(t *testing.T, bin, fake, fp string, replace bool) {
	dir := t.TempDir()
	data, dl, media := filepath.Join(dir, "data"), filepath.Join(dir, "downloads"), filepath.Join(dir, "media")
	sc := &scenario.Scenario{Version: "2026.01.01", Targets: []string{"Chrome-131 Windows-10"}, Videos: map[string]scenario.Video{
		videoID: {ID: videoID, Title: videoTitle, Duration: 60, Subtitles: []string{"en"}, Download: []scenario.Behavior{{SleepMs: 200}}},
	}}
	scPath := filepath.Join(dir, "scenario.json")
	sc.Write(scPath)
	state := filepath.Join(dir, "state")
	os.MkdirAll(state, 0o755)
	env := []string{
		"DATA_DIR=" + data, "DOWNLOAD_DIR=" + dl, "OUTPUT_DIR=" + media,
		"YTDLP_PATH=" + filepath.Join(fake, "yt-dlp"), "FFPROBE_PATH=" + filepath.Join(fake, "ffprobe"),
		"FFMPEG_PATH=" + filepath.Join(fake, "ffmpeg"), "DENO_PATH=deno-not-installed", "YTDLP_BUNDLED_DIR=" + filepath.Join(dir, "none"),
		scenario.EnvScenario + "=" + scPath, scenario.EnvState + "=" + state,
		"PASSWORD=", "MAX_CONCURRENT=", "MAX_RETRIES=", "YTDLP_CHANNEL=", "PUID=", "PGID=",
	}
	if replace {
		old := filepath.Join(media, folderName)
		os.MkdirAll(old, 0o755)
		os.WriteFile(filepath.Join(old, folderName+".mkv"), []byte("OLD"), 0o644)
		os.WriteFile(filepath.Join(old, "movie.nfo"), []byte(`<?xml version='1.0' encoding='utf-8'?>
<movie>
  <uniqueid type="fake.test" default="true">intg</uniqueid>
</movie>`), 0o644)
	}

	// Run 1: crash at the failpoint.
	p := startServer(t, bin, env, fp)
	st := p.call(t, "GET", "/api/v1/settings", nil)["settings"].(map[string]any)
	st["minFreeSpaceMB"] = 0
	p.call(t, "PUT", "/api/v1/settings", st)
	opts := map[string]any{}
	if replace {
		opts["force"] = true
	}
	job := p.call(t, "POST", "/api/v1/jobs", map[string]any{"url": scenario.VideoPrefix + videoID, "options": opts})
	if job == nil || job["id"] == nil {
		t.Fatalf("submit failed: %v\n%s", job, p.out)
	}
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("failpoint %s not reached\n%s", fp, p.out)
	}
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("server was not killed at %s: %v\n%s", fp, p.cmd.ProcessState, p.out)
	}

	// Invariants while the server is down.
	content := checkLibrary(t, media)
	if replace && content == "" {
		t.Fatal("existing library folder vanished (invariant 3)")
	}
	stDB, err := store.Open(filepath.Join(data, "ytdlp-nfo-server.db"))
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := stDB.AllArchiveKeys(context.Background())
	stDB.Close()
	inArchive := len(keys) == 1 && keys[0] == "fake "+videoID
	if inArchive && (content == "" || content == "OLD") {
		t.Fatalf("archive entry without the new item in the library (invariant 4): content=%q", content)
	}

	// Run 2: restart without failpoint; the job resumes and completes.
	p = startServer(t, bin, env, "")
	id := job["id"].(string)
	deadline := time.Now().Add(20 * time.Second)
	for {
		j := p.call(t, "GET", "/api/v1/jobs/"+id, nil)
		if j != nil && j["state"] == "completed" {
			break
		}
		if j != nil && (j["state"] == "failed" || j["state"] == "partial") {
			t.Fatalf("job failed after restart: %v\n%s", j, p.out)
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not complete after restart: %v\n%s", j, p.out)
		}
		time.Sleep(25 * time.Millisecond)
	}
	p.stop(t)

	content = checkLibrary(t, media)
	if content == "" || content == "OLD" {
		t.Fatalf("new version not in the library after resume: %q", content)
	}
	for _, f := range []string{folderName + ".en.vtt", "movie.nfo", "folder.jpg"} {
		if _, err := os.Stat(filepath.Join(media, folderName, f)); err != nil {
			t.Fatalf("missing %s after resume", f)
		}
	}
	if _, err := os.Stat(filepath.Join(media, folderName, ".ytdlp-nfo-staging")); err == nil {
		t.Fatal("staging marker left in the library")
	}
	arch, _ := os.ReadFile(filepath.Join(dl, ".ytdlp-archive.txt"))
	if strings.Count(string(arch), "fake "+videoID+"\n") != 1 {
		t.Fatalf("legacy archive file: %q", arch)
	}
	for _, leftover := range []string{filepath.Join(media, ".staging"), filepath.Join(media, ".trash"), filepath.Join(dl, ".work")} {
		if entries, _ := os.ReadDir(leftover); len(entries) > 0 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("leftovers in %s: %v", leftover, names)
		}
	}
}
