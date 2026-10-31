package library

import (
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/failpoint"
)

// Hidden directories in every library root.
const (
	StagingDirName = ".staging"
	TrashDirName   = ".trash"

	stagedMarker = ".staged"            // .staging/<uid>/.staged: staging complete
	innerMarker  = ".ytdlp-nfo-staging" // inside the staged folder until it is swapped in
)

// FileMove maps a file in the work dir to its name in the item folder.
type FileMove struct {
	Src string
	Dst string
}

// Plan describes one finalize operation. It is stored before any file is
// touched so that a crash at any point can be resumed (§4.5).
type Plan struct {
	UID     string // unique per item, e.g. "<job>-<item>"
	Root    string // library root
	Target  string // item folder relative to Root
	WorkDir string
	Files   []FileMove
	// VideoID guards replacement: an existing target is only replaced if
	// its movie.nfo names the same video.
	VideoID string
}

// ErrTargetTaken is returned when the target folder holds a different or
// unknown video. The caller must choose another target.
var ErrTargetTaken = errors.New("target folder holds a different video")

var rootLocks sync.Map // root -> *sync.Mutex

// LockRoot serializes target selection and finalize per library root so two
// items can never pick the same free folder name.
func LockRoot(root string) (unlock func()) {
	m, _ := rootLocks.LoadOrStore(filepath.Clean(root), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Finalizer moves staged items into the library.
type Finalizer struct {
	FS FS
}

// NewFinalizer returns a finalizer on the real filesystem.
func NewFinalizer() *Finalizer { return &Finalizer{FS: OSFS{}} }

func (p Plan) stagingBase() string { return filepath.Join(p.Root, StagingDirName, p.UID) }
func (p Plan) stagingItem() string { return filepath.Join(p.stagingBase(), "item") }
func (p Plan) trashPath() string   { return filepath.Join(p.Root, TrashDirName, p.UID) }
func (p Plan) targetPath() string  { return filepath.Join(p.Root, filepath.FromSlash(p.Target)) }

func validUID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (p Plan) validate() error {
	if !validUID(p.UID) {
		return fmt.Errorf("invalid finalize uid %q", p.UID)
	}
	t := filepath.ToSlash(p.Target)
	if t == "" || strings.HasPrefix(t, "/") || filepath.Clean(t) != filepath.FromSlash(t) {
		return fmt.Errorf("invalid target %q", p.Target)
	}
	for _, c := range strings.Split(t, "/") {
		if c == "" || c == "." || c == ".." || strings.HasPrefix(c, ".") {
			return fmt.Errorf("invalid target %q", p.Target)
		}
	}
	if len(p.Files) == 0 {
		return fmt.Errorf("nothing to finalize")
	}
	for _, f := range p.Files {
		if f.Dst == "" || strings.ContainsAny(f.Dst, "/\x00") || f.Dst == "." || f.Dst == ".." || f.Dst == innerMarker ||
			f.Src == "" || strings.Contains(f.Src, "..") || filepath.IsAbs(f.Src) {
			return fmt.Errorf("invalid file mapping %q -> %q", f.Src, f.Dst)
		}
	}
	return nil
}

// Finalize builds the item folder in staging and swaps it into place.
// Every step is idempotent: calling Finalize again after a crash continues
// from the first incomplete step. After a successful return the caller must
// record the result (archive + item done) and then call Cleanup.
//
// Invariants: nothing appears at the target before it is complete and
// fsynced; the target appears via a single rename (or an atomic exchange
// when replacing the same video); an existing target is never deleted
// before its replacement is in place.
func (f *Finalizer) Finalize(p Plan) error {
	if err := p.validate(); err != nil {
		return err
	}
	base, item := p.stagingBase(), p.stagingItem()
	if !exists(filepath.Join(base, stagedMarker)) {
		if err := f.stage(p); err != nil {
			return fmt.Errorf("stage: %w", err)
		}
	}
	if err := failpoint.Check("finalize:staged"); err != nil {
		return err
	}

	target := p.targetPath()
	if exists(filepath.Join(item, innerMarker)) {
		if err := MkdirAllSync(f.FS, p.Root, filepath.Dir(filepath.FromSlash(p.Target))); err != nil {
			return fmt.Errorf("create parent: %w", err)
		}
		if exists(target) {
			if p.VideoID == "" || FolderVideoID(target) != p.VideoID {
				return ErrTargetTaken
			}
			if err := f.replace(p, item, target); err != nil {
				return err
			}
		} else if err := f.FS.Rename(item, target); err != nil {
			return fmt.Errorf("move into library: %w", err)
		}
		if err := f.FS.Sync(filepath.Dir(target)); err != nil {
			return fmt.Errorf("sync library dir: %w", err)
		}
	} else if !exists(target) {
		return fmt.Errorf("finalize %s: staged folder and target are both missing", p.UID)
	}
	if err := failpoint.Check("finalize:swapped"); err != nil {
		return err
	}
	if err := f.FS.Remove(filepath.Join(target, innerMarker)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove marker: %w", err)
	}
	return f.FS.Sync(target)
}

// replace swaps the staged folder with an existing target (same video, or
// force re-download). The old version ends up in staging or trash and is
// removed by Cleanup only after the new version is recorded.
func (f *Finalizer) replace(p Plan, item, target string) error {
	err := f.FS.Exchange(item, target)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrExchangeUnsupported) {
		return fmt.Errorf("exchange: %w", err)
	}
	// Fallback for filesystems without RENAME_EXCHANGE: move the old
	// version to trash (remembering where it came from), then rename.
	if err := MkdirAllSync(f.FS, p.Root, TrashDirName); err != nil {
		return err
	}
	trash := p.trashPath()
	for i := 1; exists(trash); i++ {
		trash = fmt.Sprintf("%s.%d", p.trashPath(), i)
	}
	if err := f.FS.WriteFileSync(trash+".origin", []byte(p.Target), 0o644); err != nil {
		return err
	}
	if err := f.FS.Rename(target, trash); err != nil {
		return fmt.Errorf("move old version to trash: %w", err)
	}
	if err := failpoint.Check("finalize:trashed"); err != nil {
		return err
	}
	if err := f.FS.Rename(item, target); err != nil {
		// Put the old version back so the library is unchanged.
		if rerr := f.FS.Rename(trash, target); rerr == nil {
			_ = f.FS.Remove(trash + ".origin")
		}
		return fmt.Errorf("move into library: %w", err)
	}
	return nil
}

func (f *Finalizer) stage(p Plan) error {
	base, item := p.stagingBase(), p.stagingItem()
	if err := MkdirAllSync(f.FS, p.Root, filepath.Join(StagingDirName, p.UID, "item")); err != nil {
		return err
	}
	for _, fm := range p.Files {
		src := filepath.Join(p.WorkDir, filepath.FromSlash(fm.Src))
		dst := filepath.Join(item, fm.Dst)
		if exists(src) {
			err := f.FS.Rename(src, dst)
			if errors.Is(err, syscall.EXDEV) {
				err = f.FS.CopyFile(src, dst)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", fm.Src, err)
			}
		} else if !exists(dst) {
			return fmt.Errorf("%s: missing in work dir and staging", fm.Src)
		}
		if err := f.FS.Sync(dst); err != nil {
			return fmt.Errorf("sync %s: %w", fm.Dst, err)
		}
	}
	if err := f.FS.WriteFileSync(filepath.Join(item, innerMarker), []byte(p.UID), 0o644); err != nil {
		return err
	}
	if err := f.FS.Sync(item); err != nil {
		return err
	}
	if err := failpoint.Check("finalize:before-marker"); err != nil {
		return err
	}
	if err := f.FS.WriteFileSync(filepath.Join(base, stagedMarker), []byte(p.UID), 0o644); err != nil {
		return err
	}
	return f.FS.Sync(base)
}

// Cleanup removes staging, trash and the work dir after the result has
// been recorded.
func (f *Finalizer) Cleanup(p Plan) error {
	var errs []error
	if err := f.FS.RemoveAll(p.stagingBase()); err != nil {
		errs = append(errs, err)
	}
	matches, _ := filepath.Glob(p.trashPath() + "*")
	for _, m := range matches {
		if err := f.FS.RemoveAll(m); err != nil {
			errs = append(errs, err)
		}
	}
	if p.WorkDir != "" {
		if err := f.FS.RemoveAll(p.WorkDir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// HasStaged reports whether a complete staged copy exists for uid.
func HasStaged(root, uid string) bool {
	return exists(filepath.Join(root, StagingDirName, uid, stagedMarker))
}

// CleanupUnreferenced removes staging and trash entries whose uid is not
// referenced by any unfinished item. Displaced library folders in trash are
// restored if their original location is empty (invariant 3).
func (f *Finalizer) CleanupUnreferenced(root string, referenced func(uid string) bool, logf func(string, ...any)) {
	if entries, err := os.ReadDir(filepath.Join(root, StagingDirName)); err == nil {
		for _, e := range entries {
			if referenced(e.Name()) {
				continue
			}
			p := filepath.Join(root, StagingDirName, e.Name())
			logf("removing unreferenced staging entry %s", p)
			_ = f.FS.RemoveAll(p)
		}
	}
	trashDir := filepath.Join(root, TrashDirName)
	entries, err := os.ReadDir(trashDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".origin") {
			continue
		}
		uid, _, _ := strings.Cut(name, ".")
		if referenced(uid) {
			continue
		}
		p := filepath.Join(trashDir, name)
		origin, _ := os.ReadFile(p + ".origin")
		target := strings.TrimSpace(string(origin))
		if target != "" && !strings.Contains(target, "..") {
			dst := filepath.Join(root, filepath.FromSlash(target))
			if !exists(dst) {
				logf("restoring displaced library folder %s", dst)
				if err := MkdirAllSync(f.FS, root, filepath.Dir(filepath.FromSlash(target))); err == nil {
					if err := f.FS.Rename(p, dst); err == nil {
						_ = f.FS.Remove(p + ".origin")
						continue
					}
				}
			}
		}
		logf("removing unreferenced trash entry %s", p)
		_ = f.FS.RemoveAll(p)
		_ = f.FS.Remove(p + ".origin")
	}
}

var uniqueIDRe = regexp.MustCompile(`(?s)<uniqueid[^>]*>(.*?)</uniqueid>`)

// FolderVideoID returns the video id recorded in a library folder: the id
// part of a Reddit marker's key, else the <uniqueid> of its movie.nfo; ""
// if unknown.
func FolderVideoID(dir string) string {
	if m := ReadMarker(dir); m != nil {
		return m.ID()
	}
	f, err := os.Open(filepath.Join(dir, "movie.nfo"))
	if err != nil {
		return ""
	}
	defer f.Close()
	data, _ := io.ReadAll(io.LimitReader(f, 4<<20))
	m := uniqueIDRe.FindSubmatch(data)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(string(m[1])))
}

// ChooseTarget picks the item folder. If the rendered folder exists and holds
// the same video it is replaced; if it holds a different (or unknown) video
// a suffix " (2)", " (3)", ... is appended so nothing is overwritten (F3).
func ChooseTarget(root string, r Rendered, videoID string) (Rendered, bool, error) {
	for n := 1; n < 1000; n++ {
		cand := r
		if n > 1 {
			cand = r.WithSuffix(fmt.Sprintf(" (%d)", n))
		}
		p := filepath.Join(root, filepath.FromSlash(cand.Dir()))
		fi, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			return cand, false, nil
		}
		if err != nil {
			return cand, false, err
		}
		if fi.IsDir() && videoID != "" && FolderVideoID(p) == videoID {
			return cand, true, nil
		}
	}
	return r, false, fmt.Errorf("no free folder name for %q", r.Dir())
}
