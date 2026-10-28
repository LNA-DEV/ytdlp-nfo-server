package library

import (
	"context"
	"html"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Folder is a library folder that contains a movie.nfo or a Reddit
// marker.
type Folder struct {
	Root     string // library root
	Rel      string // folder relative to Root (slash-separated)
	VideoID  string // <uniqueid> of the NFO, or the id of the marker's key
	Domain   string // type attribute of <uniqueid> (webpage_url_domain)
	HasMedia bool   // a non-empty media file is present
	Partial  bool   // leftover .part/.ytdl files are present
}

var mediaExts = map[string]bool{
	".mkv": true, ".mp4": true, ".webm": true, ".m4v": true, ".mov": true, ".avi": true, ".flv": true, ".ts": true,
	".ogv": true, ".ogg": true, ".m4a": true, ".mp3": true, ".opus": true, ".aac": true, ".flac": true, ".wav": true,
	".mka": true, ".3gp": true, ".wmv": true,
}

// imageExts count as media only in folders with a marker: every video
// folder has a folder.jpg.
var imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".avif": true}

var uniqueIDFullRe = regexp.MustCompile(`(?s)<uniqueid([^>]*)>(.*?)</uniqueid>`)
var typeAttrRe = regexp.MustCompile(`type="([^"]*)"`)

// ReadNFOID returns the video id and domain recorded in a movie.nfo.
func ReadNFOID(path string) (id, domain string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	data, _ := io.ReadAll(io.LimitReader(f, 4<<20))
	m := uniqueIDFullRe.FindSubmatch(data)
	if m == nil {
		return "", ""
	}
	if t := typeAttrRe.FindSubmatch(m[1]); t != nil {
		domain = html.UnescapeString(string(t[1]))
	}
	return strings.TrimSpace(html.UnescapeString(string(m[2]))), domain
}

// InspectFolder reads the NFO or the Reddit marker and checks for media of
// one folder.
func InspectFolder(root, rel string) (Folder, bool) {
	dir := filepath.Join(root, filepath.FromSlash(rel))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Folder{}, false
	}
	f := Folder{Root: root, Rel: filepath.ToSlash(rel)}
	hasNFO, hasMarker, hasImage := false, false, false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(name)
		nonEmpty := func() bool { info, err := e.Info(); return err == nil && info.Size() > 0 }
		switch {
		case lower == "movie.nfo":
			hasNFO = true
		case name == MarkerName:
			hasMarker = true
		case strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".ytdl"):
			f.Partial = true
		case mediaExts[filepath.Ext(lower)]:
			f.HasMedia = f.HasMedia || nonEmpty()
		case imageExts[filepath.Ext(lower)] && lower != "folder.jpg":
			hasImage = hasImage || nonEmpty()
		}
	}
	if hasMarker {
		if m := ReadMarker(dir); m != nil {
			f.VideoID = m.ID()
			f.HasMedia = f.HasMedia || hasImage
			return f, true
		}
	}
	if !hasNFO {
		return Folder{}, false
	}
	f.VideoID, f.Domain = ReadNFOID(filepath.Join(dir, "movie.nfo"))
	return f, true
}

// ScanFolders walks root and calls fn for every folder with a movie.nfo or
// a Reddit marker.
// Hidden directories (.staging, .trash, .work, …) are skipped.
func ScanFolders(ctx context.Context, root string, fn func(Folder)) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // unreadable subfolder: skip it
		}
		if !d.IsDir() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		_, nfoErr := os.Stat(filepath.Join(p, "movie.nfo"))
		_, markerErr := os.Stat(filepath.Join(p, MarkerName))
		if nfoErr == nil || markerErr == nil {
			if f, ok := InspectFolder(root, rel); ok {
				fn(f)
			}
		}
		return nil
	})
}
