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

// Folder is a library folder that contains a movie.nfo.
type Folder struct {
	Root     string // library root
	Rel      string // folder relative to Root (slash-separated)
	VideoID  string // <uniqueid> of the NFO
	Domain   string // type attribute of <uniqueid> (webpage_url_domain)
	HasMedia bool   // a non-empty media file is present
	Partial  bool   // leftover .part/.ytdl files are present
}

var mediaExts = map[string]bool{
	".mkv": true, ".mp4": true, ".webm": true, ".m4v": true, ".mov": true, ".avi": true, ".flv": true, ".ts": true,
	".ogv": true, ".ogg": true, ".m4a": true, ".mp3": true, ".opus": true, ".aac": true, ".flac": true, ".wav": true,
	".mka": true, ".3gp": true, ".wmv": true,
}

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

// InspectFolder reads the NFO and checks for media of one folder.
func InspectFolder(root, rel string) (Folder, bool) {
	dir := filepath.Join(root, filepath.FromSlash(rel))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Folder{}, false
	}
	f := Folder{Root: root, Rel: filepath.ToSlash(rel)}
	hasNFO := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(name)
		switch {
		case lower == "movie.nfo":
			hasNFO = true
		case strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".ytdl"):
			f.Partial = true
		case mediaExts[filepath.Ext(lower)]:
			if info, err := e.Info(); err == nil && info.Size() > 0 {
				f.HasMedia = true
			}
		}
	}
	if !hasNFO {
		return Folder{}, false
	}
	f.VideoID, f.Domain = ReadNFOID(filepath.Join(dir, "movie.nfo"))
	return f, true
}

// ScanFolders walks root and calls fn for every folder with a movie.nfo.
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
		if _, err := os.Stat(filepath.Join(p, "movie.nfo")); err == nil {
			if f, ok := InspectFolder(root, rel); ok {
				fn(f)
			}
		}
		return nil
	})
}
