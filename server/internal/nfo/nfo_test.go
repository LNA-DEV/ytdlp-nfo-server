package nfo

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGoldenAgainstPython compares the Go port with the output of the
// original Python implementation (testdata/*.python.nfo, regenerated with
// testdata/gen_golden.py). Fixtures with a documented fix compare against
// *.expected.nfo instead.
func TestGoldenAgainstPython(t *testing.T) {
	fixtures, _ := filepath.Glob("testdata/*.info.json")
	if len(fixtures) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range fixtures {
		name := strings.TrimSuffix(filepath.Base(f), ".info.json")
		t.Run(name, func(t *testing.T) {
			data, _ := os.ReadFile(f)
			info, err := ParseInfo(data)
			if err != nil {
				t.Fatal(err)
			}
			got := Movie(info, Options{})
			wantPath := "testdata/" + name + ".expected.nfo"
			if _, err := os.Stat(wantPath); err != nil {
				wantPath = "testdata/" + name + ".python.nfo"
			}
			want, _ := os.ReadFile(wantPath)
			if !bytes.Equal(got, want) {
				t.Fatalf("mismatch with %s\n--- got ---\n%s\n--- want ---\n%s", wantPath, got, want)
			}
			// Every output must be well-formed XML (F19).
			if err := xml.Unmarshal(got, new(struct{})); err != nil {
				t.Fatalf("invalid XML: %v", err)
			}
		})
	}
}

func TestDateAddedAndAgeRules(t *testing.T) {
	info, _ := ParseInfo([]byte(`{"id":"x","title":"T","extractor":"KidsTube","age_limit":0}`))
	out := string(Movie(info, Options{
		AgeRules:  []AgeRule{{Match: "kids", Rating: "G"}},
		DateAdded: time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC),
	}))
	if !strings.Contains(out, "<mpaa>G</mpaa>") || !strings.Contains(out, "<dateadded>2026-10-14 12:30:00</dateadded>") {
		t.Fatal(out)
	}
}
