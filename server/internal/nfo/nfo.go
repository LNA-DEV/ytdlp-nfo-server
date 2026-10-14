// Package nfo writes Jellyfin/Kodi movie NFO files from yt-dlp info.json.
//
// It is a port of create_nfo_from_json from ytdlp-nfo and produces
// byte-identical output (verified by golden files), except for these
// documented fixes (§5.3, F19):
//   - characters that are invalid in XML 1.0 are removed,
//   - <runtime> is at least 1 for videos with a duration,
//   - <dateadded> is written when a date is given,
//   - the adult-content rule is a configurable rule list.
package nfo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// AgeRule maps an extractor-name substring to an <mpaa> rating.
type AgeRule struct {
	Match  string
	Rating string
}

// DefaultAgeRules mirror the original hard-coded rule.
var DefaultAgeRules = []AgeRule{{Match: "porn", Rating: "XXX"}, {Match: "fetish", Rating: "XXX"}}

// Options control the optional parts of the output.
type Options struct {
	AgeRules  []AgeRule // nil: DefaultAgeRules
	DateAdded time.Time // zero: no <dateadded>
}

// Info is a decoded info.json (numbers kept as json.Number so they print
// exactly like Python's str()).
type Info map[string]any

// ParseInfo decodes info.json bytes.
func ParseInfo(data []byte) (Info, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("info.json is not an object")
	}
	return Info(m), nil
}

// truthy mirrors Python truthiness for JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pyStr mirrors Python's str() for JSON values.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// or returns the first truthy value (Python "a or b").
func (in Info) or(keys ...string) any {
	for _, k := range keys {
		if v := in[k]; truthy(v) {
			return v
		}
	}
	return nil
}

func (in Info) list(key string) []any {
	if l, ok := in[key].([]any); ok {
		return l
	}
	return nil
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	}
	return 0, false
}

func formatUploadDate(v any) (year, premiered string) {
	s, ok := v.(string)
	if !ok || len(s) != 8 {
		return "", ""
	}
	t, err := time.Parse("20060102", s)
	if err != nil {
		return "", ""
	}
	return strconv.Itoa(t.Year()), t.Format("2006-01-02")
}

// element is a minimal ElementTree-like node so the output matches
// Python's serializer and indent function exactly.
type element struct {
	tag      string
	attrs    [][2]string
	text     *string
	tail     *string
	children []*element
}

func (e *element) sub(tag string, text string) *element {
	c := &element{tag: tag, text: &text}
	e.children = append(e.children, c)
	return c
}

func sp(s string) *string { return &s }

// indent ports the (slightly quirky) indent helper of ytdlp-nfo. Elements
// with children never get their own tail unless they are the last child,
// which is why "</actor><runtime>" ends up on one line.
func indent(e *element, level int) {
	i := "\n" + strings.Repeat("  ", level)
	if len(e.children) > 0 {
		if e.text == nil || strings.TrimSpace(*e.text) == "" {
			e.text = sp(i + "  ")
		}
		var last *element
		for _, c := range e.children {
			indent(c, level+1)
			last = c
		}
		if last.tail == nil || strings.TrimSpace(*last.tail) == "" {
			last.tail = sp(i)
		}
	} else if level > 0 && (e.tail == nil || strings.TrimSpace(*e.tail) == "") {
		e.tail = sp(i)
	}
}

var cdataEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
var attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "\r", "&#13;", "\n", "&#10;", "\t", "&#09;")

func serialize(b *strings.Builder, e *element) {
	b.WriteString("<" + e.tag)
	for _, a := range e.attrs {
		b.WriteString(" " + a[0] + `="` + attrEscaper.Replace(a[1]) + `"`)
	}
	if (e.text != nil && *e.text != "") || len(e.children) > 0 {
		b.WriteString(">")
		if e.text != nil {
			b.WriteString(cdataEscaper.Replace(*e.text))
		}
		for _, c := range e.children {
			serialize(b, c)
		}
		b.WriteString("</" + e.tag + ">")
	} else {
		b.WriteString(" />")
	}
	if e.tail != nil {
		b.WriteString(cdataEscaper.Replace(*e.tail))
	}
}

// CleanXMLText removes characters that are not allowed in XML 1.0 (F19).
func CleanXMLText(s string) string {
	ok := func(r rune) bool {
		return r == 0x9 || r == 0xA || r == 0xD || r >= 0x20 && r <= 0xD7FF || r >= 0xE000 && r <= 0xFFFD || r >= 0x10000 && r <= 0x10FFFF
	}
	clean := true
	for _, r := range s {
		if !ok(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	return strings.Map(func(r rune) rune {
		if ok(r) {
			return r
		}
		return -1
	}, s)
}

// Movie renders a movie NFO.
func Movie(in Info, opts Options) []byte {
	str := func(v any) string { return CleanXMLText(pyStr(v)) }

	title := "Unknown Title"
	if v := in.or("title", "fulltitle"); v != nil {
		title = pyStr(v)
	}
	originalTitle := title
	if v := in.or("fulltitle"); v != nil {
		originalTitle = pyStr(v)
	}
	plot := ""
	if v := in.or("description"); v != nil {
		plot = pyStr(v)
	}
	uploader := ""
	if v := in.or("uploader", "channel"); v != nil {
		uploader = pyStr(v)
	}
	year, premiered := formatUploadDate(in["upload_date"])

	movie := &element{tag: "movie"}
	movie.sub("title", CleanXMLText(title))
	movie.sub("originaltitle", CleanXMLText(originalTitle))
	if plot != "" {
		movie.sub("plot", CleanXMLText(plot))
		movie.sub("outline", CleanXMLText(plot))
	}
	if premiered != "" {
		movie.sub("premiered", premiered)
	}
	if year != "" {
		movie.sub("year", year)
	}
	if uploader != "" {
		actor := &element{tag: "actor"}
		movie.children = append(movie.children, actor)
		actor.sub("name", CleanXMLText(uploader))
		actor.sub("type", "Actor")
	}
	if d := in["duration"]; truthy(d) {
		if secs, ok := number(d); ok {
			minutes := int64(math.RoundToEven(secs / 60)) // Python round()
			if minutes < 1 && secs > 0 {
				minutes = 1 // fix: never 0 for videos with a duration
			}
			movie.sub("runtime", strconv.FormatInt(minutes, 10))
		}
	}
	if truthy(in["tags"]) {
		for _, t := range in.list("tags") {
			movie.sub("tag", str(t))
		}
	}
	if truthy(in["categories"]) {
		for _, c := range in.list("categories") {
			movie.sub("genre", str(c))
		}
	}
	if id := in["id"]; truthy(id) {
		domain := "unknown"
		if v := in.or("webpage_url_domain"); v != nil {
			domain = pyStr(v)
		}
		uid := movie.sub("uniqueid", str(id))
		uid.attrs = [][2]string{{"type", CleanXMLText(domain)}, {"default", "true"}}
	}
	extractor := strings.ToLower(pyStr(in["extractor"]))
	rules := opts.AgeRules
	if rules == nil {
		rules = DefaultAgeRules
	}
	rated := false
	for _, r := range rules {
		if r.Match != "" && strings.Contains(extractor, strings.ToLower(r.Match)) {
			movie.sub("mpaa", CleanXMLText(r.Rating))
			rated = true
			break
		}
	}
	if !rated && truthy(in["age_limit"]) {
		movie.sub("mpaa", str(in["age_limit"]))
	}
	if v := in.or("webpage_url"); v != nil {
		movie.sub("url", str(v))
	}
	if !opts.DateAdded.IsZero() {
		movie.sub("dateadded", opts.DateAdded.Format("2006-01-02 15:04:05"))
	}

	indent(movie, 0)
	var b strings.Builder
	b.WriteString("<?xml version='1.0' encoding='utf-8'?>\n")
	serialize(&b, movie)
	return []byte(b.String())
}
