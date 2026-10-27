package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/testutil/scenario"
)

func TestSubscriptionsAPI(t *testing.T) {
	sc := &scenario.Scenario{
		Videos: map[string]scenario.Video{"a": {ID: "a", Title: "A", Duration: 30}, "b": {ID: "b", Title: "B", Duration: 30}},
		Playlists: map[string]scenario.Playlist{"pl": {ID: "pl", Title: "List", Uploader: "Someone",
			Listings: [][]string{{"a"}, {"a"}, {"a", "b"}}}},
	}
	srv, m := newServer(t, "pw", sc)
	m.Settings.Update(context.Background(), func(s *config.Settings) error { s.Subscriptions.SiteDelaySeconds = 0; return nil })
	c := srv.Client()
	api := srv.URL + "/api/v1"
	url := scenario.PlaylistPrefix + "pl"

	resp, p := do(t, c, "POST", api+"/subscriptions/preview", "pw", map[string]string{"url": url})
	if resp.StatusCode != 200 || p["name"] != "List" || p["kind"] != "playlist" || len(p["entries"].([]any)) != 1 {
		t.Fatalf("preview: %d %v", resp.StatusCode, p)
	}
	resp, body := do(t, c, "POST", api+"/subscriptions/preview", "pw", map[string]string{"url": scenario.VideoPrefix + "a"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("video without channel: %d %v", resp.StatusCode, body)
	}

	resp, sub := do(t, c, "POST", api+"/subscriptions", "pw", map[string]any{"name": "List", "sources": []map[string]string{{"url": url}}})
	if resp.StatusCode != http.StatusCreated || sub["mode"] != "manual" {
		t.Fatalf("create: %d %v", resp.StatusCode, sub)
	}
	id := sub["id"].(string)
	resp, body = do(t, c, "POST", api+"/subscriptions", "pw", map[string]any{"name": "Again", "sources": []map[string]string{{"url": url + "/"}}})
	if resp.StatusCode != http.StatusConflict || body["subscriptionId"] != id {
		t.Fatalf("duplicate: %d %v", resp.StatusCode, body)
	}
	resp, _ = do(t, c, "POST", api+"/subscriptions", "pw", map[string]any{"name": "Bad", "mode": "sometimes", "sources": []map[string]string{{"url": url + "x"}}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid mode: %d", resp.StatusCode)
	}

	waitSub := func(cond func(map[string]any) bool) map[string]any {
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, s := do(t, c, "GET", api+"/subscriptions/"+id, "pw", nil)
			if cond(s) {
				return s
			}
			if time.Now().After(deadline) {
				t.Fatalf("subscription: %v", s)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	first := waitSub(func(s map[string]any) bool { return s["lastCheckAt"] != nil && s["state"] == "idle" })
	resp, _ = do(t, c, "PATCH", api+"/subscriptions/"+id, "pw", map[string]any{"mode": "auto", "intervalMinutes": 5})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("interval below 15 min: %d", resp.StatusCode)
	}
	resp, body = do(t, c, "PATCH", api+"/subscriptions/"+id, "pw", map[string]any{"notify": true})
	if resp.StatusCode != 200 || body["notify"] != true {
		t.Fatalf("patch: %d %v", resp.StatusCode, body)
	}
	resp, _ = do(t, c, "POST", api+"/subscriptions/check", "pw", map[string]any{})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("check all: %d", resp.StatusCode)
	}
	// Checks 2 and 3: the second finds nothing new, the third finds "b".
	waitSub(func(s map[string]any) bool { return s["lastCheckAt"] != first["lastCheckAt"] && s["state"] == "idle" })
	do(t, c, "POST", api+"/subscriptions/"+id+"/check", "pw", map[string]any{"full": true})
	waitSub(func(s map[string]any) bool { return s["counts"].(map[string]any)["new"] == 1.0 })

	resp, feed := do(t, c, "GET", api+"/feed?status=inbox", "pw", nil)
	entries := feed["entries"].([]any)
	if resp.StatusCode != 200 || len(entries) != 1 || entries[0].(map[string]any)["subscription"] != "List" {
		t.Fatalf("feed: %v", feed)
	}
	entryID := entries[0].(map[string]any)["id"].(string)
	resp, _ = do(t, c, "GET", api+"/feed?status=bogus", "pw", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad status: %d", resp.StatusCode)
	}
	resp, list := do(t, c, "GET", api+"/subscriptions?q=list", "pw", nil)
	if resp.StatusCode != 200 || len(list["subscriptions"].([]any)) != 1 || list["inbox"] != 1.0 {
		t.Fatalf("list: %v", list)
	}

	resp, dl := do(t, c, "POST", api+"/subscriptions/entries/download", "pw", map[string]any{"ids": []string{entryID}})
	if resp.StatusCode != 200 || dl["queued"] != 1.0 {
		t.Fatalf("download: %d %v", resp.StatusCode, dl)
	}
	job := dl["jobs"].([]any)[0].(map[string]any)
	if job["subscriptionId"] != id || job["band"] != "interactive" {
		t.Fatalf("job view: %v", job)
	}
	resp, _ = do(t, c, "POST", api+"/subscriptions/entries/ignore", "pw", map[string]any{"ids": []string{entryID}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ignore without ignored: %d", resp.StatusCode)
	}
	resp, ign := do(t, c, "POST", api+"/subscriptions/entries/ignore", "pw", map[string]any{"ids": []string{entryID}, "ignored": true})
	if resp.StatusCode != 200 || ign["changed"] != 1.0 {
		t.Fatalf("ignore: %v", ign)
	}
	resp, es := do(t, c, "GET", api+"/subscriptions/"+id+"/entries?status=backlog", "pw", nil)
	if resp.StatusCode != 200 || len(es["entries"].([]any)) != 1 {
		t.Fatalf("entries: %v", es)
	}

	req, _ := http.NewRequest("GET", api+"/subscriptions/"+id+"/log", nil)
	req.Header.Set("Authorization", "Bearer pw")
	lr, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	logText, _ := io.ReadAll(lr.Body)
	lr.Body.Close()
	if !strings.Contains(string(logText), "Checked 1 source") || !strings.Contains(string(logText), "(full listing)") {
		t.Fatalf("log:\n%s", logText)
	}

	resp, _ = do(t, c, "DELETE", api+"/subscriptions/"+id+"?cancelQueued=1", "pw", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp, _ = do(t, c, "GET", api+"/subscriptions/"+id, "pw", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete: %d", resp.StatusCode)
	}
	resp, _ = do(t, c, "GET", fmt.Sprintf("%s/subscriptions/%s/entries", api, id), "pw", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("entries after delete: %d", resp.StatusCode)
	}
}
