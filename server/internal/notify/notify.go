// Package notify sends job notifications (webhook, ntfy, Gotify, Discord)
// and asks Jellyfin to pick up new library items (§13).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
)

// JobInfo is the payload of a notification.
type JobInfo struct {
	ID      int64  `json:"id"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Done    int    `json:"done"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
	Total   int    `json:"total"`
	Link    string `json:"link,omitempty"`
}

// Notifier sends notifications.
type Notifier struct {
	Client *http.Client
	Logf   func(format string, args ...any)
}

func (n *Notifier) client() *http.Client {
	if n.Client != nil {
		return n.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (n *Notifier) logf(format string, args ...any) {
	if n.Logf != nil {
		n.Logf(format, args...)
	}
}

func (n *Notifier) post(ctx context.Context, url, contentType string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "ytdlp-nfo-server")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := n.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

// Message returns title and text for a job (one summary per playlist).
func Message(j JobInfo) (string, string) {
	name := j.Title
	if name == "" {
		name = j.URL
	}
	switch j.State {
	case "completed":
		if j.Done == 0 && j.Skipped > 0 {
			return "Already in library", name
		}
		if j.Total > 1 {
			return "Download completed", fmt.Sprintf("%s: %d downloaded, %d already in library", name, j.Done, j.Skipped)
		}
		return "Download completed", name
	case "partial":
		return "Download partly failed", fmt.Sprintf("%s: %d of %d items failed", name, j.Failed, j.Total)
	default:
		msg := name
		if j.Error != "" {
			msg += ": " + j.Error
		}
		return "Download failed", msg
	}
}

// Send delivers a job notification to all configured targets.
func (n *Notifier) Send(ctx context.Context, s config.Notifications, j JobInfo) {
	failed := j.State != "completed"
	if failed && !s.OnFailed || !failed && !s.OnCompleted {
		return
	}
	title, text := Message(j)
	if j.Link != "" {
		text += "\n" + j.Link
	}
	webhook, _ := json.Marshal(map[string]any{"event": "job." + j.State, "title": title, "message": text, "job": j})
	n.deliver(ctx, s, title, text, failed, webhook)
}

// SubscriptionInfo identifies the subscription of a notification.
type SubscriptionInfo struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Subscription events (subscriptions concept §11).
const (
	EventSubscriptionNew   = "subscription.new"
	EventSubscriptionError = "subscription.error"
	EventSubscriptionCap   = "subscription.cap"
)

// SendSubscription delivers a subscription notification to all configured
// targets. The caller decides whether it is wanted (notify, onFailed).
func (n *Notifier) SendSubscription(ctx context.Context, s config.Notifications, event, title, text string, sub SubscriptionInfo) {
	webhook, _ := json.Marshal(map[string]any{"event": event, "title": title, "message": text, "subscription": sub})
	n.deliver(ctx, s, title, text, event != EventSubscriptionNew, webhook)
}

// deliver sends one message to every configured target.
func (n *Notifier) deliver(ctx context.Context, s config.Notifications, title, text string, failed bool, webhook []byte) {
	if s.WebhookURL != "" {
		if err := n.post(ctx, s.WebhookURL, "application/json", webhook, nil); err != nil {
			n.logf("notify: webhook: %v", err)
		}
	}
	if s.NtfyURL != "" {
		tags := "white_check_mark"
		if failed {
			tags = "warning"
		}
		if err := n.post(ctx, s.NtfyURL, "text/plain", []byte(text), map[string]string{"Title": title, "Tags": tags}); err != nil {
			n.logf("notify: ntfy: %v", err)
		}
	}
	if s.GotifyURL != "" {
		prio := 5
		if failed {
			prio = 8
		}
		body, _ := json.Marshal(map[string]any{"title": title, "message": text, "priority": prio})
		u := strings.TrimRight(s.GotifyURL, "/") + "/message"
		if err := n.post(ctx, u, "application/json", body, map[string]string{"X-Gotify-Key": s.GotifyToken}); err != nil {
			n.logf("notify: gotify: %v", err)
		}
	}
	if s.DiscordWebhookURL != "" {
		body, _ := json.Marshal(map[string]any{"content": "**" + title + "**\n" + text})
		if err := n.post(ctx, s.DiscordWebhookURL, "application/json", body, nil); err != nil {
			n.logf("notify: discord: %v", err)
		}
	}
}

// MapPath rewrites a server path to the path Jellyfin sees.
func MapPath(p string, maps []config.PathMap) string {
	for _, m := range maps {
		from := filepath.Clean(m.From)
		if m.From == "" {
			continue
		}
		if p == from || strings.HasPrefix(p, from+"/") {
			return filepath.Join(m.To, strings.TrimPrefix(p, from))
		}
	}
	return p
}

// JellyfinRefresh reports a new or changed path so Jellyfin picks it up
// within seconds instead of waiting for a full library scan.
func (n *Notifier) JellyfinRefresh(ctx context.Context, j config.Jellyfin, path string) error {
	if j.URL == "" || j.APIKey == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"Updates": []map[string]string{{"Path": MapPath(path, j.PathMap), "UpdateType": "Created"}}})
	u := strings.TrimRight(j.URL, "/") + "/Library/Media/Updated"
	return n.post(ctx, u, "application/json", body, map[string]string{
		"X-Emby-Token":  j.APIKey,
		"Authorization": `MediaBrowser Token="` + j.APIKey + `"`,
	})
}
