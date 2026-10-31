// Package jschallenge answers the JavaScript check Reddit shows some
// clients instead of a page (Reddit concept §5.4): an inline script fills
// a hidden form and submits it. The script runs in goja, a JavaScript
// engine without network or file access, inside a child process of the
// server with a time and memory limit, against a minimal stand-in for
// document, the form and URLSearchParams. Nothing else of the page runs.
//
// The child is the server binary itself, started with ChildEnv set; main
// and the test binaries that answer checks call ChildMain then.
package jschallenge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// ChildEnv makes the binary run as the child that runs one check.
const ChildEnv = "YTDLP_NFO_JS_CHALLENGE_CHILD"

// Limits of one check.
const (
	maxScript   = 64 << 10 // inline scripts of the page, together
	maxOutput   = 64 << 10
	childLimit  = 10 * time.Second // the child, from start to exit
	scriptLimit = 3 * time.Second  // the script inside the child
	memoryLimit = 256 << 20        // heap of the child
)

// Input is what the child gets: the inline scripts, the page's location
// and its form.
type Input struct {
	Scripts   []string `json:"scripts"`
	Href      string   `json:"href"`
	Search    string   `json:"search"`
	Pathname  string   `json:"pathname"`
	Form      Form     `json:"form"`
	UserAgent string   `json:"userAgent"` // the client's, for navigator
}

// Form is the challenge's form.
type Form struct {
	Method string  `json:"method"`
	Action string  `json:"action"`
	Inputs []Field `json:"inputs"`
}

// Field is one form field.
type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type,omitempty"`
}

// Output is what the child returns: the fields the script submitted.
type Output struct {
	Submitted bool    `json:"submitted"`
	Fields    []Field `json:"fields"`
}

// Extract reads the inline scripts and the first form of a check page.
func Extract(page []byte, pageURL *url.URL) (*Input, error) {
	in := &Input{Href: pageURL.String(), Pathname: pageURL.EscapedPath()}
	if pageURL.RawQuery != "" {
		in.Search = "?" + pageURL.RawQuery
	}
	z := html.NewTokenizer(bytes.NewReader(page))
	inScript, inForm, forms, size := false, false, 0, 0
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			if forms == 0 {
				return nil, errors.New("the page has no form")
			}
			if len(in.Scripts) == 0 {
				return nil, errors.New("the page has no inline script")
			}
			return in, nil
		case html.TextToken:
			if inScript {
				s := string(z.Text())
				if size += len(s); size > maxScript {
					return nil, fmt.Errorf("the page's scripts are larger than %d KB", maxScript>>10)
				}
				in.Scripts = append(in.Scripts, s)
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, attrs := tagAttrs(z)
			switch {
			case name == "script" && tt == html.StartTagToken:
				_, external := attrs["src"]
				inScript = !external
			case name == "form" && forms == 0:
				forms++
				inForm = tt == html.StartTagToken
				in.Form.Method, in.Form.Action = strings.ToUpper(attrs["method"]), attrs["action"]
			case name == "input" && inForm && attrs["name"] != "":
				in.Form.Inputs = append(in.Form.Inputs, Field{Name: attrs["name"], Value: attrs["value"], Type: attrs["type"]})
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script":
				inScript = false
			case "form":
				inForm = false
			}
		}
	}
}

func tagAttrs(z *html.Tokenizer) (string, map[string]string) {
	name, more := z.TagName()
	attrs := map[string]string{}
	for more {
		var k, v []byte
		k, v, more = z.TagAttr()
		attrs[string(k)] = string(v)
	}
	return string(name), attrs
}

// Solver answers checks in a child process.
type Solver struct {
	// Exe is the binary to start with ChildEnv; "" uses the running one.
	Exe string
	// UserAgent is the client's, so navigator matches the requests.
	UserAgent string
}

// Solve runs the check of a page and returns the URL the script's form
// submission asks for. It only follows a GET form on the page's own host.
func (s Solver) Solve(ctx context.Context, page []byte, pageURL *url.URL) (string, error) {
	in, err := Extract(page, pageURL)
	if err != nil {
		return "", err
	}
	in.UserAgent = s.UserAgent
	out, err := s.run(ctx, in)
	if err != nil {
		return "", err
	}
	return SubmitURL(pageURL, in.Form, out.Fields)
}

func (s Solver) run(ctx context.Context, in *Input) (*Output, error) {
	exe := s.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, err
		}
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, childLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe)
	// The child gets nothing of the server's environment: no settings,
	// no credentials.
	cmd.Env = []string{ChildEnv + "=1"}
	cmd.Dir = os.TempDir()
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = maxOutput, 4<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("the check did not finish in time")
		}
		return nil, fmt.Errorf("the check failed: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	var out Output
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("the check's answer is unreadable: %v", err)
	}
	if !out.Submitted {
		return nil, errors.New("the script did not submit its form")
	}
	return &out, nil
}

// SubmitURL builds the URL of a GET form submission on the page's host,
// with the fields in the order the script left them.
func SubmitURL(pageURL *url.URL, f Form, fields []Field) (string, error) {
	if f.Method != "" && f.Method != "GET" {
		return "", fmt.Errorf("the form is sent with %s", f.Method)
	}
	action, err := pageURL.Parse(f.Action)
	if err != nil {
		return "", err
	}
	if action.Scheme != "https" || !strings.EqualFold(action.Host, pageURL.Host) {
		return "", fmt.Errorf("the form leads to another site (%s)", action.Host)
	}
	var q []string
	for _, fl := range fields {
		if fl.Name != "" {
			q = append(q, url.QueryEscape(fl.Name)+"="+url.QueryEscape(fl.Value))
		}
	}
	action.RawQuery, action.Fragment = strings.Join(q, "&"), ""
	return action.String(), nil
}

// limitedBuffer keeps at most max bytes.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

var _ io.Writer = (*limitedBuffer)(nil)
