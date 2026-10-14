// Package library renders library paths, moves finished items into the
// library atomically and maintains the archive (§4.5, §5.4).
package library

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var timestampRe = regexp.MustCompile(`[0-9]+(?::[0-9]+)+`)

// SanitizeFilename is a port of yt-dlp's sanitize_filename with its default
// arguments (restricted=False, is_id=NO_DEFAULT), so names match what yt-dlp
// produced for v1 libraries: path-unsafe characters become their full-width
// counterparts ("?" -> "？", "/" -> "⧸").
//
// One deliberate addition: leading dots are removed so a title can never
// produce a hidden folder or a "." / ".." path component.
func SanitizeFilename(s string) string {
	if s == "" {
		return ""
	}
	s = timestampRe.ReplaceAllStringFunc(s, func(m string) string {
		return strings.ReplaceAll(m, ":", "_")
	})
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString("\x00 ")
		case r == '/':
			b.WriteRune('⧸')
		case r == '\\':
			b.WriteRune('⧹')
		case strings.ContainsRune(`"*:<>?|`, r):
			b.WriteRune(r + 0xfee0)
		case r < 32 || r == 127:
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	res := collapseRepeatedSubstitutes(b.String())
	res = stripSubstitutes(res)
	res = strings.ReplaceAll(res, "\x00", "")
	if res == "" {
		res = "_"
	}
	res = strings.TrimLeft(res, ".")
	if res == "" {
		res = "_"
	}
	return res
}

// collapseRepeatedSubstitutes implements re.sub(r'(\0.)(?:(?=\1)..)+', r'\1').
func collapseRepeatedSubstitutes(s string) string {
	if !strings.Contains(s, "\x00") {
		return s
	}
	rs := []rune(s)
	out := make([]rune, 0, len(rs))
	for i := 0; i < len(rs); {
		if rs[i] == 0 && i+1 < len(rs) {
			pair := rs[i : i+2]
			out = append(out, pair...)
			i += 2
			for i+1 < len(rs) && rs[i] == pair[0] && rs[i+1] == pair[1] {
				i += 2
			}
			continue
		}
		out = append(out, rs[i])
		i++
	}
	return string(out)
}

// stripTail reports whether rs consists only of "\0X" pairs and [ _-]
// characters and ends with a pair (the tail alternative of yt-dlp's STRIP_RE).
func stripTail(rs []rune) bool {
	lastPair := false
	for i := 0; i < len(rs); {
		switch {
		case rs[i] == 0 && i+1 < len(rs):
			i += 2
			lastPair = true
		case rs[i] == ' ' || rs[i] == '_' || rs[i] == '-':
			i++
			lastPair = false
		default:
			return false
		}
	}
	return lastPair
}

// stripSubstitutes removes substitute characters from the start and end,
// like yt-dlp's re.sub(f'^\0.{STRIP_RE}|{STRIP_RE}\0.$', ...) with
// STRIP_RE = r'(?:\0.|[ _-])*' and an empty replacement.
func stripSubstitutes(s string) string {
	if !strings.Contains(s, "\x00") {
		return s
	}
	rs := []rune(s)
	start := 0
	if len(rs) >= 2 && rs[0] == 0 {
		i := 2
		for i < len(rs) {
			if rs[i] == 0 && i+1 < len(rs) {
				i += 2
			} else if rs[i] == ' ' || rs[i] == '_' || rs[i] == '-' {
				i++
			} else {
				break
			}
		}
		start = i
	}
	end := len(rs)
	for p := start; p < len(rs); p++ {
		if stripTail(rs[p:]) {
			end = p
			break
		}
	}
	return string(rs[start:end])
}

// TruncateBytes shortens s to at most n bytes without splitting a rune.
func TruncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	s = s[:n]
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r == utf8.RuneError && size <= 1 {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}
