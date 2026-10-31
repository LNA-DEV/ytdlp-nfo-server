package browser

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	http "github.com/Noooste/fhttp"
)

// Jar keeps the cookies Reddit sets for a logged-out visitor, across
// requests and restarts (§5.1). Only *.reddit.com cookies are kept; media
// hosts and Arctic Shift get none.
type Jar struct {
	mu      sync.Mutex
	path    string
	cookies map[string]jarCookie
	fixed   map[string]string // always sent, e.g. over18=1
}

type jarCookie struct {
	Value   string    `json:"value"`
	Expires time.Time `json:"expires,omitzero"`
}

// NewJar loads the jar from path ("" keeps it in memory).
func NewJar(path string, fixed map[string]string) *Jar {
	j := &Jar{path: path, cookies: map[string]jarCookie{}, fixed: fixed}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &j.cookies)
		}
	}
	return j
}

func redditHost(u *url.URL) bool {
	h := strings.ToLower(u.Hostname())
	return h == "reddit.com" || strings.HasSuffix(h, ".reddit.com")
}

// SetCookies implements http.CookieJar.
func (j *Jar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	if !redditHost(u) || len(cookies) == 0 {
		return
	}
	now := time.Now()
	j.mu.Lock()
	defer j.mu.Unlock()
	changed := false
	for _, c := range cookies {
		if c.Name == "" || j.fixed[c.Name] != "" {
			continue
		}
		exp := c.Expires
		if c.MaxAge > 0 {
			exp = now.Add(time.Duration(c.MaxAge) * time.Second)
		}
		if c.MaxAge < 0 || (!exp.IsZero() && exp.Before(now)) {
			if _, ok := j.cookies[c.Name]; ok {
				delete(j.cookies, c.Name)
				changed = true
			}
			continue
		}
		if old, ok := j.cookies[c.Name]; !ok || old.Value != c.Value || !old.Expires.Equal(exp) {
			j.cookies[c.Name] = jarCookie{Value: c.Value, Expires: exp}
			changed = true
		}
	}
	if changed {
		j.saveLocked()
	}
}

// Cookies implements http.CookieJar.
func (j *Jar) Cookies(u *url.URL) []*http.Cookie {
	if !redditHost(u) {
		return nil
	}
	now := time.Now()
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []*http.Cookie
	for name, c := range j.cookies {
		if !c.Expires.IsZero() && c.Expires.Before(now) {
			continue
		}
		out = append(out, &http.Cookie{Name: name, Value: c.Value})
	}
	for name, v := range j.fixed {
		out = append(out, &http.Cookie{Name: name, Value: v})
	}
	return out
}

// Reset forgets every cookie Reddit set (after a block, §5.4).
func (j *Jar) Reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cookies = map[string]jarCookie{}
	j.saveLocked()
}

// Len returns the number of stored cookies.
func (j *Jar) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.cookies)
}

func (j *Jar) saveLocked() {
	if j.path == "" {
		return
	}
	data, err := json.Marshal(j.cookies)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(j.path), 0o700)
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, j.path)
}
