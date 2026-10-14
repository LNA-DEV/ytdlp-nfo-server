package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/manager"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/scenario"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp/versions"
	"github.com/LNA-DEV/ytdlp-nfo-server/web"
)

var fakeDir string

func TestMain(m *testing.M) {
	dir, err := testutil.FakeBin()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type resolver struct{ b versions.Binary }

func (r resolver) Acquire(context.Context, string) (versions.Binary, func(), error) {
	return r.b, func() {}, nil
}
func (r resolver) IsLatest(context.Context, versions.Binary) bool { return true }

func newServer(t *testing.T, password string, sc *scenario.Scenario) (*httptest.Server, *manager.Manager) {
	t.Helper()
	dir := t.TempDir()
	scPath := filepath.Join(dir, "scenario.json")
	if sc == nil {
		sc = &scenario.Scenario{}
	}
	sc.Version = "2026.01.01"
	if sc.Videos == nil {
		sc.Videos = map[string]scenario.Video{}
	}
	sc.Videos["one"] = scenario.Video{ID: "one", Title: "One", Duration: 30, Download: []scenario.Behavior{{SleepMs: 200}}}
	sc.Write(scPath)
	t.Setenv(scenario.EnvScenario, scPath)
	t.Setenv(scenario.EnvState, dir)
	env := &config.Env{
		DataDir: filepath.Join(dir, "data"), DownloadDir: filepath.Join(dir, "dl"), OutputDir: filepath.Join(dir, "media"),
		Password: password, FFmpegPath: filepath.Join(fakeDir, "ffmpeg"), FFprobePath: filepath.Join(fakeDir, "ffprobe"),
	}
	m, err := manager.New(manager.Options{
		Env: env, DisableBackground: true, Logf: t.Logf,
		Resolver:    resolver{versions.Binary{ID: "stable/1", Channel: "stable", Tag: "1", Version: "1", Path: filepath.Join(fakeDir, "yt-dlp")}},
		TweakPolicy: func(p *scheduler.Policy) { p.Base, p.Cap = 10*time.Millisecond, 50*time.Millisecond },
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Settings.Update(context.Background(), func(s *config.Settings) error { s.MinFreeSpaceMB = 0; return nil })
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(m, web.Static()))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		m.Shutdown(ctx)
	})
	return srv, m
}

func do(t *testing.T, c *http.Client, method, url, auth string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, url, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &out)
	return resp, out
}

// The browser add-on: POST /api/download {url} with "Bearer <password>"
// must keep returning 201, then 409 for a duplicate, 401 for a wrong
// password.
func TestLegacyAddonFlow(t *testing.T) {
	srv, m := newServer(t, "secret-pw", nil)
	c := srv.Client()
	url := scenario.VideoPrefix + "one"

	resp, _ := do(t, c, "POST", srv.URL+"/api/download", "wrong", map[string]string{"url": url})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp, body := do(t, c, "POST", srv.URL+"/api/download", "secret-pw", map[string]string{"url": url})
	if resp.StatusCode != http.StatusCreated || body["id"] == nil || body["status"] == nil {
		t.Fatalf("submit: %d %v", resp.StatusCode, body)
	}
	resp, body = do(t, c, "POST", srv.URL+"/api/download", "secret-pw", map[string]string{"url": url})
	if resp.StatusCode != http.StatusConflict || body["error"] == nil {
		t.Fatalf("duplicate: %d %v", resp.StatusCode, body)
	}
	// Legacy list/detail shapes.
	req, _ := http.NewRequest("GET", srv.URL+"/api/jobs", nil)
	req.Header.Set("Authorization", "Bearer secret-pw")
	r2, _ := c.Do(req)
	var list []map[string]any
	json.NewDecoder(r2.Body).Decode(&list)
	r2.Body.Close()
	if len(list) != 1 || list[0]["options"].(map[string]any)["format"] != "mkv" {
		t.Fatalf("legacy list: %v", list)
	}
	id := list[0]["id"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, d := do(t, c, "GET", srv.URL+"/api/jobs/"+id, "secret-pw", nil)
		if d["status"] == "completed" {
			if out, ok := d["output"].([]any); !ok || len(out) == 0 {
				t.Fatalf("legacy detail without output: %v", d)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job not completed: %v", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, body = do(t, c, "GET", srv.URL+"/api/version", "secret-pw", nil)
	if resp.StatusCode != 200 || body["ytdlp-nfo-server"] == nil {
		t.Fatalf("version: %v", body)
	}
	// Legacy bulk.
	// The first job is completed, so the URL may be submitted again (v1
	// semantics); the second copy in the same request is a duplicate.
	resp, body = do(t, c, "POST", srv.URL+"/api/download/bulk", "secret-pw", map[string]any{"urls": []string{url, url + "#again", "ftp://bad", ""}})
	if resp.StatusCode != 200 || body["created"].(float64) != 1 || body["duplicates"].(float64) != 1 || body["errors"].(float64) != 1 {
		t.Fatalf("bulk: %v", body)
	}
	resp, _ = do(t, c, "DELETE", srv.URL+"/api/jobs/"+id, "secret-pw", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if n, _ := m.Counts(context.Background()); n["completed"] > 1 {
		t.Fatalf("counts after delete: %v", n)
	}
}

func TestSessionsTokensAndRateLimit(t *testing.T) {
	srv, _ := newServer(t, "secret-pw", nil)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	resp, body := do(t, c, "GET", srv.URL+"/api/v1/auth", "", nil)
	if resp.StatusCode != 200 || body["authRequired"] != true || body["authenticated"] != false {
		t.Fatalf("auth status: %v", body)
	}
	resp, _ = do(t, c, "GET", srv.URL+"/api/v1/jobs", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: %d", resp.StatusCode)
	}
	resp, _ = do(t, c, "POST", srv.URL+"/api/v1/login", "", map[string]string{"password": "secret-pw"})
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	var sc *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie {
			sc = ck
		}
	}
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode || strings.Contains(sc.Value, "secret") {
		t.Fatalf("session cookie: %+v", sc)
	}
	resp, _ = do(t, c, "GET", srv.URL+"/api/v1/jobs?state=active", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("session list: %d", resp.StatusCode)
	}
	// Cross-origin state change with the cookie is rejected.
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/jobs", strings.NewReader(`{"url":"https://fake.test/v/one"}`))
	req.Header.Set("Origin", "https://evil.example")
	r2, _ := c.Do(req)
	r2.Body.Close()
	if r2.StatusCode != http.StatusForbidden {
		t.Fatalf("CSRF: %d", r2.StatusCode)
	}
	// The add-on (extension origin, Bearer token) works even with a cookie.
	req, _ = http.NewRequest("POST", srv.URL+"/api/download", strings.NewReader(`{"url":"https://fake.test/v/one"}`))
	req.Header.Set("Origin", "moz-extension://abc")
	req.Header.Set("Authorization", "Bearer secret-pw")
	r2, _ = c.Do(req)
	r2.Body.Close()
	if r2.StatusCode != http.StatusCreated {
		t.Fatalf("add-on with cookie: %d", r2.StatusCode)
	}
	// API token.
	resp, body = do(t, c, "POST", srv.URL+"/api/v1/tokens", "", map[string]string{"name": "addon"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("token: %d %v", resp.StatusCode, body)
	}
	token := body["token"].(string)
	plain := &http.Client{}
	resp, _ = do(t, plain, "GET", srv.URL+"/api/v1/system", token, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("token auth: %d", resp.StatusCode)
	}
	// Logout invalidates the session.
	do(t, c, "POST", srv.URL+"/api/v1/logout", "", nil)
	resp, _ = do(t, c, "GET", srv.URL+"/api/v1/jobs", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
	// Login rate limiting.
	for i := 0; i < 5; i++ {
		do(t, plain, "POST", srv.URL+"/api/v1/login", "", map[string]string{"password": "nope"})
	}
	resp, _ = do(t, plain, "POST", srv.URL+"/api/v1/login", "", map[string]string{"password": "secret-pw"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate limit: %d", resp.StatusCode)
	}
	// Health check needs no auth.
	resp, _ = do(t, plain, "GET", srv.URL+"/healthz", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}

func TestV1JobsAndEvents(t *testing.T) {
	srv, _ := newServer(t, "", nil)
	c := srv.Client()

	// Global event stream.
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/events", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)
	evResp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer evResp.Body.Close()
	if ct := evResp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	seen := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(evResp.Body)
		for sc.Scan() {
			if ev, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				seen <- ev
			}
		}
		close(seen)
	}()

	resp, body := do(t, c, "POST", srv.URL+"/api/v1/jobs", "", map[string]any{"url": scenario.VideoPrefix + "one", "options": map[string]any{"preset": "max720"}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	id := body["id"].(string)
	if body["options"].(map[string]any)["preset"] != "max720" {
		t.Fatalf("options: %v", body["options"])
	}
	resp, body = do(t, c, "POST", srv.URL+"/api/v1/jobs", "", map[string]any{"url": "-o /tmp/x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("argument injection accepted: %d %v", resp.StatusCode, body)
	}
	got := map[string]bool{}
	timeout := time.After(10 * time.Second)
	for !(got["job"] && got["counts"] && got["item"] && got["system"]) {
		select {
		case ev, ok := <-seen:
			if !ok {
				t.Fatal("stream closed")
			}
			got[ev] = true
		case <-timeout:
			t.Fatalf("events seen: %v", got)
		}
	}
	// Wait for completion, then check job detail, log and tabs.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, j := do(t, c, "GET", srv.URL+"/api/v1/jobs/"+id, "", nil)
		if j["state"] == "completed" {
			items := j["items"].([]any)
			if len(items) != 1 || items[0].(map[string]any)["path"] != "One [one]" {
				t.Fatalf("items: %v", items)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not completed: %v", j)
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, body = do(t, c, "GET", srv.URL+"/api/v1/jobs?state=completed", "", nil)
	if jobs := body["jobs"].([]any); len(jobs) != 1 || body["counts"].(map[string]any)["completed"].(float64) != 1 {
		t.Fatalf("completed tab: %v", body)
	}
	r, _ := c.Get(srv.URL + "/api/v1/jobs/" + id + "/log")
	logData, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(logData), "Finalized: One [one]") {
		t.Fatalf("log: %s", logData)
	}
	// Archive endpoints.
	resp, body = do(t, c, "GET", srv.URL+"/api/v1/archive?q=one", "", nil)
	if body["total"].(float64) != 1 {
		t.Fatalf("archive: %v", body)
	}
	req, _ = http.NewRequest("DELETE", srv.URL+"/api/v1/archive/"+strings.ReplaceAll("fake one", " ", "%20"), nil)
	r, _ = c.Do(req)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("archive delete: %d", r.StatusCode)
	}
	// Clear completed.
	resp, body = do(t, c, "POST", srv.URL+"/api/v1/jobs/clear?state=completed", "", nil)
	if body["removed"].(float64) != 1 {
		t.Fatalf("clear: %v", body)
	}
	// Static UI is served.
	r, _ = c.Get(srv.URL + "/")
	html, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(html), "ytdlp-nfo Server") || r.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("UI not served")
	}
}

func TestSettingsValidationAndSecrets(t *testing.T) {
	srv, _ := newServer(t, "", nil)
	c := srv.Client()
	_, body := do(t, c, "GET", srv.URL+"/api/v1/settings", "", nil)
	st := body["settings"].(map[string]any)
	st["naming"] = map[string]any{"preset": "custom", "template": "{title}.{ext}"}
	resp, _ := do(t, c, "PUT", srv.URL+"/api/v1/settings", "", st)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid template accepted: %d", resp.StatusCode)
	}
	st["naming"] = map[string]any{"preset": "default"}
	st["jellyfin"] = map[string]any{"url": "http://jf:8096", "apiKey": "supersecret"}
	resp, body = do(t, c, "PUT", srv.URL+"/api/v1/settings", "", st)
	if resp.StatusCode != 200 {
		t.Fatalf("save: %d %v", resp.StatusCode, body)
	}
	if body["settings"].(map[string]any)["jellyfin"].(map[string]any)["apiKey"] == "supersecret" {
		t.Fatal("secret returned by the API")
	}
	// Cookie upload never returns the content.
	resp, body = do(t, c, "POST", srv.URL+"/api/v1/cookies", "", map[string]string{"name": "acct", "content": "# Netscape HTTP Cookie File\n.example.com\tTRUE\t/\tTRUE\t0\tsid\tSECRETVALUE\n"})
	if resp.StatusCode != 200 {
		t.Fatalf("cookies: %d %v", resp.StatusCode, body)
	}
	r, _ := c.Get(srv.URL + "/api/v1/cookies")
	data, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if strings.Contains(string(data), "SECRETVALUE") || !strings.Contains(string(data), "acct") {
		t.Fatalf("cookie list: %s", data)
	}
}

func TestArchiveCheckRoutes(t *testing.T) {
	srv, m := newServer(t, "", nil)
	c := srv.Client()
	m.Store.ImportArchiveKeys(context.Background(), []string{"youtube gone1"}, "import")
	resp, body := do(t, c, "POST", srv.URL+"/api/v1/archive/check", "", nil)
	if resp.StatusCode != 200 || body["missing"].(float64) != 1 {
		t.Fatalf("check: %d %v", resp.StatusCode, body)
	}
	probs := body["problems"].([]any)
	if probs[0].(map[string]any)["url"] != "https://www.youtube.com/watch?v=gone1" {
		t.Fatalf("problems: %v", probs)
	}
	resp, _ = do(t, c, "POST", srv.URL+"/api/v1/archive/fix", "", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("fix without keys: %d", resp.StatusCode)
	}
	resp, body = do(t, c, "POST", srv.URL+"/api/v1/archive/fix", "", map[string]any{"keys": []string{"youtube gone1"}})
	if resp.StatusCode != 200 || body["removed"].(float64) != 1 {
		t.Fatalf("fix: %d %v", resp.StatusCode, body)
	}
	if ok, _ := m.Store.HasArchive(context.Background(), "youtube gone1"); ok {
		t.Fatal("entry not removed")
	}
}
