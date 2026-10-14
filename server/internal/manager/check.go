package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
)

// Archive check statuses.
const (
	CheckMissing    = "missing"    // no library folder for the entry
	CheckIncomplete = "incomplete" // folder with movie.nfo but no (complete) media
)

// ArchiveProblem is an archive entry whose video is not (completely) in
// the library, e.g. after v1 deleted a partly failed playlist (F1).
type ArchiveProblem struct {
	Key         string    `json:"key"`
	Extractor   string    `json:"extractor"`
	VideoID     string    `json:"videoId"`
	Status      string    `json:"status"`
	Path        string    `json:"path,omitempty"`     // library folder (found or expected)
	Library     string    `json:"library,omitempty"`  // library root of Path
	Stranded    string    `json:"stranded,omitempty"` // files found outside the library (v1 job dir)
	URL         string    `json:"url,omitempty"`      // URL for a re-download, if known
	Source      string    `json:"source"`
	FinalizedAt time.Time `json:"finalizedAt"`
}

// ArchiveCheck is the result of comparing the archive with the library.
type ArchiveCheck struct {
	Entries    int              `json:"entries"`
	OK         int              `json:"ok"`
	Missing    int              `json:"missing"`
	Incomplete int              `json:"incomplete"`
	Folders    int              `json:"folders"`
	Libraries  []string         `json:"libraries"`
	Duration   float64          `json:"duration"`
	Problems   []ArchiveProblem `json:"problems"`
}

// urlTemplates rebuild a URL from an archive key for common sites, so
// entries imported from v1 (which have no URL) can be re-downloaded.
var urlTemplates = map[string]string{
	"youtube":     "https://www.youtube.com/watch?v=%s",
	"vimeo":       "https://vimeo.com/%s",
	"dailymotion": "https://www.dailymotion.com/video/%s",
}

func templateURL(extractor, id string) string {
	t, ok := urlTemplates[strings.ToLower(extractor)]
	if !ok || id == "" || strings.ContainsAny(id, "/?#&% ") {
		return ""
	}
	return fmt.Sprintf(t, id)
}

// CheckArchive compares every archive entry with the library: an entry is
// ok if a library folder holds its movie.nfo (matched by the stored path or
// by the video id in <uniqueid>) and a non-empty media file. Nothing is
// changed; FixArchive acts on the result.
func (m *Manager) CheckArchive(ctx context.Context) (*ArchiveCheck, error) {
	start := time.Now()
	entries, err := m.Store.AllArchive(ctx)
	if err != nil {
		return nil, err
	}
	res := &ArchiveCheck{Entries: len(entries), Problems: []ArchiveProblem{}}

	roots := m.libraryRoots()
	byID := map[string][]library.Folder{}
	byPath := map[string]library.Folder{}
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		res.Libraries = append(res.Libraries, root)
		err := library.ScanFolders(ctx, root, func(f library.Folder) {
			res.Folders++
			byPath[filepath.Join(f.Root, filepath.FromSlash(f.Rel))] = f
			if f.VideoID != "" {
				byID[f.VideoID] = append(byID[f.VideoID], f)
			}
		})
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", root, err)
		}
	}

	// v1 job dirs (DOWNLOAD_DIR/<number>/) may still hold items that never
	// reached a separate library. Reported only, never touched.
	stranded := map[string]string{}
	if m.Env.OutputDir != "" {
		if dirs, err := os.ReadDir(m.Env.DownloadDir); err == nil {
			for _, d := range dirs {
				if _, err := strconv.Atoi(d.Name()); err != nil || !d.IsDir() {
					continue
				}
				jobDir := filepath.Join(m.Env.DownloadDir, d.Name())
				_ = library.ScanFolders(ctx, jobDir, func(f library.Folder) {
					if f.VideoID != "" && f.HasMedia {
						stranded[f.VideoID] = filepath.Join(jobDir, filepath.FromSlash(f.Rel))
					}
				})
			}
		}
	}

	urls, _ := m.Store.URLsByArchiveKey(ctx)
	for _, e := range entries {
		var cands []library.Folder
		if e.Path != "" {
			for _, root := range roots {
				if f, ok := byPath[filepath.Join(root, filepath.FromSlash(e.Path))]; ok {
					cands = append(cands, f)
				}
			}
		}
		cands = append(cands, byID[e.VideoID]...)
		ok := false
		for _, f := range cands {
			if f.HasMedia && !f.Partial {
				ok = true
				break
			}
		}
		if ok {
			res.OK++
			continue
		}
		p := ArchiveProblem{Key: e.Key, Extractor: e.Extractor, VideoID: e.VideoID, Source: e.Source, FinalizedAt: e.FinalizedAt, Path: e.Path}
		if len(cands) > 0 {
			p.Status = CheckIncomplete
			p.Path, p.Library = cands[0].Rel, cands[0].Root
			res.Incomplete++
		} else {
			p.Status = CheckMissing
			p.Stranded = stranded[e.VideoID]
			res.Missing++
		}
		p.URL = urls[e.Key]
		if p.URL == "" {
			p.URL = templateURL(e.Extractor, e.VideoID)
		}
		res.Problems = append(res.Problems, p)
	}
	sort.SliceStable(res.Problems, func(i, j int) bool { return res.Problems[i].Key < res.Problems[j].Key })
	res.Duration = time.Since(start).Seconds()
	m.logf("library check: %d archive entries, %d ok, %d missing, %d incomplete (%d folders scanned)",
		res.Entries, res.OK, res.Missing, res.Incomplete, res.Folders)
	return res, nil
}

// ArchiveFixResult reports what FixArchive did.
type ArchiveFixResult struct {
	Removed int      `json:"removed"`
	Queued  int      `json:"queued"`
	NoURL   []string `json:"noUrl"`
	Errors  []string `json:"errors"`
}

// FixArchive removes the given entries from the archive (database and
// .ytdlp-archive.txt) so the videos can be downloaded again. With
// redownload, a job is created for every entry whose URL is known.
// Library files are never touched.
func (m *Manager) FixArchive(ctx context.Context, keys []string, redownload bool) (*ArchiveFixResult, error) {
	res := &ArchiveFixResult{NoURL: []string{}, Errors: []string{}}
	if len(keys) == 0 {
		return res, nil
	}
	entries := map[string]store.ArchiveEntry{}
	for _, k := range keys {
		if e, err := m.Store.GetArchive(ctx, k); err == nil {
			entries[k] = *e
		}
	}
	urls, _ := m.Store.URLsByArchiveKey(ctx)
	n, err := m.Store.RemoveArchiveKeys(ctx, keys)
	if err != nil {
		return nil, err
	}
	res.Removed = n
	if err := m.Archive.RemoveMany(keys); err != nil {
		res.Errors = append(res.Errors, "updating "+m.Archive.Path+": "+err.Error())
	}
	m.logf("library check: removed %d archive entries", n)
	if !redownload {
		return res, nil
	}
	for _, k := range keys {
		e, ok := entries[k]
		if !ok {
			continue
		}
		u := urls[k]
		if u == "" {
			u = templateURL(e.Extractor, e.VideoID)
		}
		if u == "" {
			res.NoURL = append(res.NoURL, k)
			continue
		}
		_, err := m.Submit(ctx, u, config.OptionsRequest{})
		var dup *DuplicateError
		switch {
		case err == nil, errors.As(err, &dup):
			res.Queued++
		default:
			res.Errors = append(res.Errors, k+": "+err.Error())
		}
	}
	return res, nil
}
