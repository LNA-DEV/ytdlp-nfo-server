package library

import (
	"fmt"
	"path"
	"strings"
)

// Byte limits for path components (§5.4). A file name keeps room for
// subtitle suffixes such as ".en-US.vtt" and for collision suffixes " (12)".
const (
	maxComponentBytes = 255
	suffixReserve     = 8  // " (NN)"
	extReserve        = 32 // ".<lang>.<subext>"
)

// Fields allowed in naming templates. Rendering happens entirely in Go;
// yt-dlp never sees titles in a template (F17).
var allowedFields = map[string]bool{
	"title": true, "id": true, "uploader": true, "channel": true, "upload_date": true,
	"upload_year": true, "playlist_title": true, "extractor": true, "ext": true,
}

type part struct {
	lit      string
	field    string
	optional bool
}

// Template is a parsed naming template such as
// "{playlist_title?}/{title} [{id}]/{title} [{id}].{ext}".
type Template struct {
	raw   string
	comps [][]part
}

// ParseTemplate validates and parses a naming template. The last component
// is the media file and must end in ".{ext}"; at least one folder component
// is required because every item gets its own folder (folder.jpg, movie.nfo).
func ParseTemplate(raw string) (*Template, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("template is empty")
	}
	if strings.HasPrefix(raw, "/") {
		return nil, fmt.Errorf("template must be relative")
	}
	t := &Template{raw: raw}
	for _, c := range strings.Split(raw, "/") {
		if c == "" || c == "." || c == ".." {
			return nil, fmt.Errorf("invalid path component %q", c)
		}
		parts, err := parseComponent(c)
		if err != nil {
			return nil, err
		}
		t.comps = append(t.comps, parts)
	}
	if len(t.comps) < 2 {
		return nil, fmt.Errorf("template needs a folder per item, e.g. {title}/{title}.{ext}")
	}
	last := t.comps[len(t.comps)-1]
	if n := len(last); n < 2 || last[n-1].field != "ext" || !strings.HasSuffix(last[n-2].lit, ".") {
		return nil, fmt.Errorf("file name must end with .{ext}")
	}
	for _, comps := range t.comps[:len(t.comps)-1] {
		for _, p := range comps {
			if p.field == "ext" {
				return nil, fmt.Errorf("{ext} is only allowed at the end")
			}
		}
	}
	return t, nil
}

func parseComponent(c string) ([]part, error) {
	var parts []part
	for c != "" {
		i := strings.IndexByte(c, '{')
		if i < 0 {
			if strings.ContainsRune(c, '}') {
				return nil, fmt.Errorf("unbalanced '}' in template")
			}
			parts = append(parts, part{lit: c})
			break
		}
		if i > 0 {
			if strings.ContainsRune(c[:i], '}') {
				return nil, fmt.Errorf("unbalanced '}' in template")
			}
			parts = append(parts, part{lit: c[:i]})
		}
		j := strings.IndexByte(c[i:], '}')
		if j < 0 {
			return nil, fmt.Errorf("unbalanced '{' in template")
		}
		name := c[i+1 : i+j]
		opt := strings.HasSuffix(name, "?")
		name = strings.TrimSuffix(name, "?")
		if !allowedFields[name] {
			return nil, fmt.Errorf("unknown template field {%s}", name)
		}
		parts = append(parts, part{field: name, optional: opt})
		c = c[i+j+1:]
	}
	return parts, nil
}

// String returns the template source.
func (t *Template) String() string { return t.raw }

// Rendered is a rendered library path.
type Rendered struct {
	Dirs []string // folder components; the last one is the item folder
	Base string   // media file name without extension
	Ext  string
}

// Dir returns the item folder relative to the library root.
func (r Rendered) Dir() string { return path.Join(r.Dirs...) }

// FileName returns the media file name.
func (r Rendered) FileName() string { return r.Base + "." + r.Ext }

// WithSuffix appends a collision suffix such as " (2)" to the item folder
// and the file base name.
func (r Rendered) WithSuffix(s string) Rendered {
	dirs := append([]string(nil), r.Dirs...)
	dirs[len(dirs)-1] += s
	return Rendered{Dirs: dirs, Base: r.Base + s, Ext: r.Ext}
}

// Render renders the template. Field values are sanitized like yt-dlp
// does; missing required fields become "NA"; a component made only of
// empty optional fields is dropped.
func (t *Template) Render(fields map[string]string) Rendered {
	vals := map[string]string{}
	for k, v := range fields {
		if k == "ext" {
			vals[k] = v
			continue
		}
		vals[k] = SanitizeFilename(strings.TrimSpace(v))
	}
	var r Rendered
	for ci, comp := range t.comps {
		isFile := ci == len(t.comps)-1
		if isFile {
			// Render without ".{ext}", which is always the trailing two parts.
			base := append([]part(nil), comp[:len(comp)-2]...)
			base = append(base, part{lit: strings.TrimSuffix(comp[len(comp)-2].lit, ".")})
			limit := maxComponentBytes - suffixReserve - extReserve
			r.Base = renderComponent(base, vals, limit)
			if r.Base == "" {
				r.Base = "_"
			}
			r.Ext = vals["ext"]
			continue
		}
		s := renderComponent(comp, vals, maxComponentBytes-suffixReserve)
		if s == "" {
			if allOptional(comp) {
				continue
			}
			s = "_"
		}
		r.Dirs = append(r.Dirs, s)
	}
	if len(r.Dirs) == 0 {
		r.Dirs = []string{"_"}
	}
	return r
}

func allOptional(comp []part) bool {
	for _, p := range comp {
		if p.field == "" || !p.optional {
			return false
		}
	}
	return true
}

// renderComponent renders parts and trims the longest field value until the
// component fits into limit bytes.
func renderComponent(comp []part, vals map[string]string, limit int) string {
	local := map[string]string{}
	hasValue := false
	for _, p := range comp {
		if p.field == "" {
			continue
		}
		v := vals[p.field]
		if v == "" && !p.optional {
			v = "NA"
		}
		if v != "" {
			hasValue = true
		}
		local[p.field] = v
	}
	if !hasValue && allOptional(comp) {
		return ""
	}
	render := func() string {
		var b strings.Builder
		for _, p := range comp {
			if p.field == "" {
				b.WriteString(p.lit)
			} else {
				b.WriteString(local[p.field])
			}
		}
		return b.String()
	}
	s := render()
	for i := 0; len(s) > limit && i < 8; i++ {
		longest := ""
		for _, p := range comp { // deterministic: first longest field wins
			if p.field != "" && len(local[p.field]) > len(local[longest]) {
				longest = p.field
			}
		}
		over := len(s) - limit
		if longest == "" {
			break
		}
		local[longest] = strings.TrimRight(TruncateBytes(local[longest], len(local[longest])-over), " ")
		s = render()
	}
	if len(s) > limit {
		s = TruncateBytes(s, limit)
	}
	s = strings.TrimLeft(s, ".")
	return s
}
