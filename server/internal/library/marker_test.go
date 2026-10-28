package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMarkerFolders(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, files map[string]string) string {
		dir := filepath.Join(root, rel)
		os.MkdirAll(dir, 0o755)
		for n, c := range files {
			os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644)
		}
		return dir
	}
	m := &Marker{Key: "redditgallery 5f2ab1c09d3e4a7b", Post: "1abc23", Kind: "gallery", Created: time.Unix(1700000000, 0).UTC(),
		Files: []MarkerFile{{Name: "G - 01.jpg", Media: "redditmedia q8", SHA256: "aa"}},
		Known: []MarkerKnown{{Position: 2, Media: "redditmedia c4", Path: "pics/Other/Other.jpg"}}}
	data, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	gallery := mk("pics/G [1abc23]", map[string]string{MarkerName: string(data), "G - 01.jpg": "jpeg"})
	mk("pics/Empty [x]", map[string]string{MarkerName: `{"key":"redditmedia x","files":[]}`})
	// A video folder whose media is missing: folder.jpg is not media.
	mk("vids/V [v1]", map[string]string{"movie.nfo": `<movie><uniqueid type="fake.test" default="true">v1</uniqueid></movie>`, "folder.jpg": "jpeg"})
	mk("pics/Broken", map[string]string{MarkerName: "{not json"})

	got := ReadMarker(gallery)
	if got == nil || got.ID() != "5f2ab1c09d3e4a7b" || len(got.Known) != 1 || got.Known[0].Path != "pics/Other/Other.jpg" || !got.Created.Equal(m.Created) {
		t.Fatalf("marker: %+v", got)
	}
	if id := FolderVideoID(gallery); id != "5f2ab1c09d3e4a7b" {
		t.Fatalf("FolderVideoID = %q", id)
	}
	found := map[string]Folder{}
	if err := ScanFolders(context.Background(), root, func(f Folder) { found[f.Rel] = f }); err != nil {
		t.Fatal(err)
	}
	if f := found["pics/G [1abc23]"]; !f.HasMedia || f.VideoID != "5f2ab1c09d3e4a7b" {
		t.Fatalf("gallery folder: %+v", f)
	}
	if f, ok := found["pics/Empty [x]"]; !ok || f.HasMedia || f.VideoID != "x" {
		t.Fatalf("empty folder: %+v", f)
	}
	if f := found["vids/V [v1]"]; f.HasMedia || f.VideoID != "v1" {
		t.Fatalf("video folder: %+v", f)
	}
	if _, ok := found["pics/Broken"]; ok || len(found) != 3 {
		t.Fatalf("found: %v", found)
	}

	// A forced re-download replaces the gallery's folder instead of adding " (2)".
	r := Rendered{Dirs: []string{"pics", "G [1abc23]"}, Base: "G [1abc23]", Ext: "jpg"}
	target, replace, err := ChooseTarget(root, r, "5f2ab1c09d3e4a7b")
	if err != nil || !replace || target.Dir() != "pics/G [1abc23]" {
		t.Fatalf("ChooseTarget: %v %v %v", target.Dir(), replace, err)
	}
	target, replace, _ = ChooseTarget(root, r, "other")
	if replace || target.Dir() != "pics/G [1abc23] (2)" {
		t.Fatalf("other post: %v %v", target.Dir(), replace)
	}
}
