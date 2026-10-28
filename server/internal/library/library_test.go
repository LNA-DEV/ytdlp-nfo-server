package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/failpoint"
)

func TestSanitizeMatchesYtdlp(t *testing.T) {
	for _, c := range sanitizeCases {
		if got := SanitizeFilename(c[0]); got != c[1] {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestTemplateRender(t *testing.T) {
	def, err := ParseTemplate("{playlist_title?}/{title} [{id}]/{title} [{id}].{ext}")
	if err != nil {
		t.Fatal(err)
	}
	r := def.Render(map[string]string{"title": "What? A/B", "id": "abc", "ext": "mkv"})
	if r.Dir() != "What？ A⧸B [abc]" || r.FileName() != "What？ A⧸B [abc].mkv" {
		t.Fatalf("got %q / %q", r.Dir(), r.FileName())
	}
	r = def.Render(map[string]string{"title": "T", "id": "x", "ext": "mp4", "playlist_title": "My List"})
	if r.Dir() != "My List/T [x]" {
		t.Fatalf("playlist dir = %q", r.Dir())
	}
	legacy, _ := ParseTemplate("{playlist_title?}/{title}/{title}.{ext}")
	r = legacy.Render(map[string]string{"title": "%(title)s ..x", "id": "x", "ext": "mkv"})
	if r.Dir() != "%(title)s ..x" {
		t.Fatalf("template syntax in title must stay literal: %q", r.Dir())
	}
	r = legacy.Render(map[string]string{"title": "..", "ext": "mkv"})
	if r.Dir() == ".." || strings.HasPrefix(r.Dir(), ".") {
		t.Fatalf("dot component: %q", r.Dir())
	}
	if s := r.WithSuffix(" (2)"); s.Dir() != r.Dir()+" (2)" || s.Base != r.Base+" (2)" {
		t.Fatalf("suffix: %+v", s)
	}
}

func TestTemplateByteLimits(t *testing.T) {
	def, _ := ParseTemplate("{playlist_title?}/{title} [{id}]/{title} [{id}].{ext}")
	long := strings.Repeat("日本語", 60) // 540 bytes
	r := def.Render(map[string]string{"title": long, "id": "theid", "ext": "mkv", "playlist_title": long})
	for _, c := range r.Dirs {
		if len(c) > 255 {
			t.Fatalf("component %d bytes", len(c))
		}
		if !strings.HasPrefix(long, strings.TrimSuffix(strings.TrimSuffix(c, " [theid]"), "")) && !strings.HasSuffix(c, "[theid]") {
			t.Fatalf("unexpected component %q", c)
		}
	}
	if !strings.HasSuffix(r.Dirs[1], " [theid]") {
		t.Fatalf("id suffix must survive trimming: %q", r.Dirs[1])
	}
	name := r.FileName()
	if len(name)+len(".en-US.vtt") > 255 || len(name)+len(" (99)") > 255 {
		t.Fatalf("file name leaves no room for suffixes: %d bytes", len(name))
	}
}

func TestTemplateValidation(t *testing.T) {
	bad := []string{"", "/abs/{title}.{ext}", "{title}.{ext}", "{title}/{bogus}.{ext}", "{title}/{title}", "../{title}/{title}.{ext}", "{title/{title}.{ext}", "{ext}/{title}.{ext}"}
	for _, b := range bad {
		if _, err := ParseTemplate(b); err == nil {
			t.Errorf("ParseTemplate(%q) accepted", b)
		}
	}
}

// --- finalize ---

type env struct {
	root, work string
	f          *Finalizer
}

func newEnv(t *testing.T) *env {
	d := t.TempDir()
	e := &env{root: filepath.Join(d, "lib"), work: filepath.Join(d, "work"), f: NewFinalizer()}
	os.MkdirAll(e.root, 0o755)
	os.MkdirAll(e.work, 0o755)
	return e
}

func (e *env) writeWork(t *testing.T, files map[string]string) []FileMove {
	var moves []FileMove
	for src, content := range files {
		os.WriteFile(filepath.Join(e.work, src), []byte(content), 0o644)
		dst := src
		if strings.HasPrefix(src, "media.") {
			dst = "Video [v1]." + strings.TrimPrefix(src, "media.")
		}
		moves = append(moves, FileMove{Src: src, Dst: dst})
	}
	return moves
}

func readDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, en.Name()))
		out[en.Name()] = string(b)
	}
	return out
}

const nfoV1 = `<?xml version='1.0' encoding='utf-8'?>
<movie>
  <uniqueid type="example.com" default="true">v1</uniqueid>
</movie>`

func (e *env) plan(files []FileMove) Plan {
	return Plan{UID: "1-1", Root: e.root, Target: "PL/Video [v1]", WorkDir: e.work, Files: files, VideoID: "v1"}
}

func TestFinalizeFresh(t *testing.T) {
	e := newEnv(t)
	p := e.plan(e.writeWork(t, map[string]string{"media.mkv": "MEDIA", "movie.nfo": nfoV1, "folder.jpg": "JPG"}))
	if err := e.f.Finalize(p); err != nil {
		t.Fatal(err)
	}
	got := readDir(t, filepath.Join(e.root, "PL/Video [v1]"))
	if got["Video [v1].mkv"] != "MEDIA" || got["folder.jpg"] != "JPG" || len(got) != 3 {
		t.Fatalf("library content = %v", got)
	}
	if err := e.f.Cleanup(p); err != nil {
		t.Fatal(err)
	}
	if exists(e.work) || exists(filepath.Join(e.root, StagingDirName, "1-1")) {
		t.Fatal("cleanup incomplete")
	}
}

func TestFinalizeReplacesSameVideoOnly(t *testing.T) {
	e := newEnv(t)
	old := filepath.Join(e.root, "PL/Video [v1]")
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, "movie.nfo"), []byte(nfoV1), 0o644)
	os.WriteFile(filepath.Join(old, "Video [v1].mkv"), []byte("OLD"), 0o644)

	p := e.plan(e.writeWork(t, map[string]string{"media.mkv": "NEW", "movie.nfo": nfoV1}))
	if err := e.f.Finalize(p); err != nil {
		t.Fatal(err)
	}
	if got := readDir(t, old); got["Video [v1].mkv"] != "NEW" {
		t.Fatalf("not replaced: %v", got)
	}
	e.f.Cleanup(p)

	// A folder of a different video is never replaced.
	e2 := newEnv(t)
	other := filepath.Join(e2.root, "PL/Video [v1]")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "Video [v1].mkv"), []byte("OTHER"), 0o644)
	p2 := e2.plan(e2.writeWork(t, map[string]string{"media.mkv": "NEW", "movie.nfo": nfoV1}))
	if err := e2.f.Finalize(p2); !errors.Is(err, ErrTargetTaken) {
		t.Fatalf("err = %v, want ErrTargetTaken", err)
	}
	if got := readDir(t, other); got["Video [v1].mkv"] != "OTHER" {
		t.Fatal("foreign folder modified")
	}
	// ChooseTarget picks a suffixed folder instead.
	tpl, _ := ParseTemplate("{playlist_title?}/{title} [{id}]/{title} [{id}].{ext}")
	r := tpl.Render(map[string]string{"playlist_title": "PL", "title": "Video", "id": "v1", "ext": "mkv"})
	c, replace, err := ChooseTarget(e2.root, r, "v1")
	if err != nil || replace || c.Dir() != "PL/Video [v1] (2)" {
		t.Fatalf("ChooseTarget = %q replace=%v err=%v", c.Dir(), replace, err)
	}
	// Staging stays valid; finalize to the new target.
	p2.Target = c.Dir()
	if err := e2.f.Finalize(p2); err != nil {
		t.Fatal(err)
	}
}

// TestFinalizeCrashResume simulates a crash at every failpoint (the step
// returns early without cleanup, like a kill) and resumes.
func TestFinalizeCrashResume(t *testing.T) {
	points := []string{"finalize:before-marker", "finalize:staged", "finalize:trashed", "finalize:swapped"}
	for _, replaceOld := range []bool{false, true} {
		for _, exchange := range []bool{true, false} {
			for _, fp := range points {
				name := fp
				if replaceOld {
					name += "/replace"
				}
				if !exchange {
					name += "/no-exchange"
				}
				t.Run(name, func(t *testing.T) {
					e := newEnv(t)
					if !exchange {
						e.f.FS = noExchangeFS{}
					}
					target := filepath.Join(e.root, "PL/Video [v1]")
					if replaceOld {
						os.MkdirAll(target, 0o755)
						os.WriteFile(filepath.Join(target, "movie.nfo"), []byte(nfoV1), 0o644)
						os.WriteFile(filepath.Join(target, "Video [v1].mkv"), []byte("OLD"), 0o644)
					}
					p := e.plan(e.writeWork(t, map[string]string{"media.mkv": "NEW", "movie.nfo": nfoV1}))
					restore := failpoint.SetHook(func(n string) error {
						if n == fp {
							return errors.New("crash")
						}
						return nil
					})
					err := e.f.Finalize(p)
					restore()
					crashed := err != nil
					// Invariant: the target holds either the complete old or
					// the complete new version, never a partial one.
					if exists(target) {
						got := readDir(t, target)
						media := got["Video [v1].mkv"]
						if media != "OLD" && media != "NEW" {
							t.Fatalf("partial target after crash: %v", got)
						}
						if media == "NEW" && got["movie.nfo"] != nfoV1 {
							t.Fatalf("new target incomplete: %v", got)
						}
					} else if replaceOld && !(fp == "finalize:trashed" && !exchange) {
						t.Fatal("old version vanished")
					}
					if !crashed && fp != "finalize:trashed" {
						t.Fatalf("failpoint %s not reached", fp)
					}
					// Resume.
					if err := e.f.Finalize(p); err != nil {
						t.Fatalf("resume: %v", err)
					}
					got := readDir(t, target)
					if got["Video [v1].mkv"] != "NEW" {
						t.Fatalf("after resume: %v", got)
					}
					if _, ok := got[innerMarker]; ok {
						t.Fatal("staging marker left in library")
					}
					e.f.Cleanup(p)
					if exists(filepath.Join(e.root, StagingDirName, p.UID)) || exists(p.trashPath()) {
						t.Fatal("leftovers after cleanup")
					}
				})
			}
		}
	}
}

type noExchangeFS struct{ OSFS }

func (noExchangeFS) Exchange(a, b string) error { return ErrExchangeUnsupported }

// faultFS injects errors into selected operations.
type faultFS struct {
	OSFS
	rename   func(o, n string) error
	copyFile func(src, dst string) error
	exchange func(a, b string) error
}

func (f faultFS) Rename(o, n string) error {
	if f.rename != nil {
		if err := f.rename(o, n); err != nil {
			return err
		}
	}
	return f.OSFS.Rename(o, n)
}

func (f faultFS) CopyFile(src, dst string) error {
	if f.copyFile != nil {
		return f.copyFile(src, dst)
	}
	return f.OSFS.CopyFile(src, dst)
}

func (f faultFS) Exchange(a, b string) error {
	if f.exchange != nil {
		return f.exchange(a, b)
	}
	return f.OSFS.Exchange(a, b)
}

func TestFinalizeEXDEVFallsBackToCopy(t *testing.T) {
	e := newEnv(t)
	e.f.FS = faultFS{rename: func(o, n string) error {
		if strings.HasPrefix(o, e.work) {
			return &os.LinkError{Op: "rename", Old: o, New: n, Err: syscall.EXDEV}
		}
		return nil
	}}
	p := e.plan(e.writeWork(t, map[string]string{"media.mkv": "MEDIA", "movie.nfo": nfoV1}))
	// The copy keeps the modification time, as a rename does (a Reddit
	// post's time, Reddit concept §8).
	posted := time.Date(2026, 10, 27, 18, 3, 11, 0, time.UTC)
	os.Chtimes(filepath.Join(e.work, "media.mkv"), posted, posted)
	if err := e.f.Finalize(p); err != nil {
		t.Fatal(err)
	}
	if got := readDir(t, filepath.Join(e.root, "PL/Video [v1]")); got["Video [v1].mkv"] != "MEDIA" {
		t.Fatalf("got %v", got)
	}
	if fi, err := os.Stat(filepath.Join(e.root, "PL/Video [v1]", "Video [v1].mkv")); err != nil || !fi.ModTime().Equal(posted) {
		t.Fatalf("mtime after copy: %v %v", fi.ModTime(), err)
	}
}

func TestFinalizeDiskFullKeepsLibrary(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(e.root, "PL/Video [v1]")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(target, "movie.nfo"), []byte(nfoV1), 0o644)
	os.WriteFile(filepath.Join(target, "Video [v1].mkv"), []byte("OLD"), 0o644)
	e.f.FS = faultFS{
		rename: func(o, n string) error {
			if strings.HasPrefix(o, e.work) {
				return &os.LinkError{Op: "rename", Old: o, New: n, Err: syscall.EXDEV}
			}
			return nil
		},
		copyFile: func(src, dst string) error {
			os.WriteFile(dst, []byte("PART"), 0o644)
			return &os.PathError{Op: "write", Path: dst, Err: syscall.ENOSPC}
		},
	}
	p := e.plan(e.writeWork(t, map[string]string{"media.mkv": "NEW", "movie.nfo": nfoV1}))
	if err := e.f.Finalize(p); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("err = %v", err)
	}
	if got := readDir(t, target); got["Video [v1].mkv"] != "OLD" {
		t.Fatalf("library changed: %v", got)
	}
	// Space freed: retry succeeds and replaces the partial staged copy.
	e.f.FS = faultFS{rename: func(o, n string) error {
		if strings.HasPrefix(o, e.work) {
			return &os.LinkError{Op: "rename", Old: o, New: n, Err: syscall.EXDEV}
		}
		return nil
	}}
	if err := e.f.Finalize(p); err != nil {
		t.Fatal(err)
	}
	if got := readDir(t, target); got["Video [v1].mkv"] != "NEW" {
		t.Fatalf("got %v", got)
	}
}

func TestFinalizePermissionDeniedKeepsOld(t *testing.T) {
	e := newEnv(t)
	target := filepath.Join(e.root, "PL/Video [v1]")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(target, "movie.nfo"), []byte(nfoV1), 0o644)
	os.WriteFile(filepath.Join(target, "Video [v1].mkv"), []byte("OLD"), 0o644)
	stagedItem := ""
	e.f.FS = faultFS{
		exchange: func(a, b string) error { return ErrExchangeUnsupported },
		rename: func(o, n string) error {
			if strings.Contains(o, StagingDirName) && n == target {
				stagedItem = o
				return &os.LinkError{Op: "rename", Old: o, New: n, Err: syscall.EACCES}
			}
			return nil
		},
	}
	p := e.plan(e.writeWork(t, map[string]string{"media.mkv": "NEW", "movie.nfo": nfoV1}))
	if err := e.f.Finalize(p); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("err = %v", err)
	}
	if stagedItem == "" {
		t.Fatal("fault not triggered")
	}
	if got := readDir(t, target); got["Video [v1].mkv"] != "OLD" {
		t.Fatalf("old version not restored: %v", got)
	}
}

func TestCleanupUnreferencedRestoresTrash(t *testing.T) {
	e := newEnv(t)
	trash := filepath.Join(e.root, TrashDirName, "9-9")
	os.MkdirAll(trash, 0o755)
	os.WriteFile(filepath.Join(trash, "a.mkv"), []byte("KEEP"), 0o644)
	os.WriteFile(trash+".origin", []byte("PL/Lost"), 0o644)
	os.MkdirAll(filepath.Join(e.root, StagingDirName, "8-8", "item"), 0o755)
	os.MkdirAll(filepath.Join(e.root, StagingDirName, "7-7", "item"), 0o755)
	e.f.CleanupUnreferenced(e.root, func(uid string) bool { return uid == "7-7" }, t.Logf)
	if got := readDir(t, filepath.Join(e.root, "PL/Lost")); got["a.mkv"] != "KEEP" {
		t.Fatal("displaced folder not restored")
	}
	if exists(filepath.Join(e.root, StagingDirName, "8-8")) {
		t.Fatal("unreferenced staging not removed")
	}
	if !exists(filepath.Join(e.root, StagingDirName, "7-7")) {
		t.Fatal("referenced staging removed")
	}
}

func TestArchiveFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".ytdlp-archive.txt")
	os.WriteFile(p, []byte("youtube a\nyoutube b"), 0o644) // no trailing newline
	a := &ArchiveFile{Path: p}
	if err := a.Append("vimeo c"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "youtube a\nyoutube b\nvimeo c\n" {
		t.Fatalf("got %q", b)
	}
	a.Remove("youtube b")
	b, _ = os.ReadFile(p)
	if string(b) != "youtube a\nvimeo c\n" {
		t.Fatalf("got %q", b)
	}
}

func TestScanFolders(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, files map[string]string) {
		dir := filepath.Join(root, rel)
		os.MkdirAll(dir, 0o755)
		for n, c := range files {
			os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644)
		}
	}
	nfo := func(id string) string {
		return `<movie><uniqueid type="www.example.com" default="true">` + id + `</uniqueid></movie>`
	}
	mk("PL/A [a]", map[string]string{"movie.nfo": nfo("a"), "A [a].mkv": "x"})
	mk("B", map[string]string{"movie.nfo": nfo("b")})                                           // no media
	mk("C", map[string]string{"movie.nfo": nfo("c&amp;d"), "C.mp4": "x", "C.f1.mp4.part": "y"}) // partial
	mk("D", map[string]string{"movie.nfo": nfo("d"), "D.mkv": ""})                              // empty media
	mk(".staging/1-1/item", map[string]string{"movie.nfo": nfo("hidden"), "x.mkv": "x"})
	mk("E", map[string]string{"E.mkv": "x"}) // no NFO
	got := map[string]Folder{}
	if err := ScanFolders(context.Background(), root, func(f Folder) { got[f.Rel] = f }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("folders = %v", got)
	}
	if f := got["PL/A [a]"]; f.VideoID != "a" || f.Domain != "www.example.com" || !f.HasMedia || f.Partial {
		t.Fatalf("A = %+v", f)
	}
	if got["B"].HasMedia || got["D"].HasMedia {
		t.Fatal("folder without (non-empty) media reported as complete")
	}
	if f := got["C"]; f.VideoID != "c&d" || !f.Partial {
		t.Fatalf("C = %+v", f)
	}
}
