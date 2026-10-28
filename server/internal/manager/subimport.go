package manager

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/subscriptions"
)

// Import and export formats (subscriptions concept §11).
const (
	FormatURLs       = "urls"
	FormatOPML       = "opml"
	FormatYouTubeCSV = "youtube-csv"
)

// maxImport limits one import.
const maxImport = 1000

// importItem is one channel or playlist of an import file.
type importItem struct {
	URL   string
	Title string
}

// ImportSkip reports a source that was not imported.
type ImportSkip struct {
	URL            string `json:"url"`
	Reason         string `json:"reason"`
	SubscriptionID string `json:"subscriptionId,omitempty"`
	Name           string `json:"name,omitempty"`
}

// ImportResult lists the created subscriptions and the skipped sources.
type ImportResult struct {
	Created []SubscriptionView `json:"created"`
	Skipped []ImportSkip       `json:"skipped"`
}

// youTubeFeedChannel returns the channel id of a YouTube feed URL
// (…/feeds/videos.xml?channel_id=UC…).
func youTubeFeedChannel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.HasSuffix(strings.ToLower(u.Hostname()), "youtube.com") {
		return ""
	}
	if id := u.Query().Get("channel_id"); subscriptions.IsChannelID(id) {
		return id
	}
	return ""
}

type opmlOutline struct {
	Text     string        `xml:"text,attr"`
	Title    string        `xml:"title,attr"`
	XMLURL   string        `xml:"xmlUrl,attr"`
	HTMLURL  string        `xml:"htmlUrl,attr"`
	Children []opmlOutline `xml:"outline"`
}

func parseImport(format, data string) ([]importItem, error) {
	var items []importItem
	switch format {
	case FormatURLs:
		for _, line := range strings.Split(data, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			items = append(items, importItem{URL: line})
		}
	case FormatOPML:
		var doc struct {
			Body struct {
				Outlines []opmlOutline `xml:"outline"`
			} `xml:"body"`
		}
		if err := xml.Unmarshal([]byte(data), &doc); err != nil {
			return nil, invalid("not an OPML file: %v", err)
		}
		var walk func([]opmlOutline)
		walk = func(os []opmlOutline) {
			for _, o := range os {
				title := strings.TrimSpace(o.Title)
				if title == "" {
					title = strings.TrimSpace(o.Text)
				}
				// A feed URL stands for the whole channel: the Videos tab,
				// unless htmlUrl names another tab (our own export does).
				_, tab, isTab := subscriptions.YouTubeChannel(strings.TrimSpace(o.HTMLURL))
				switch id := youTubeFeedChannel(o.XMLURL); {
				case id != "" && !(isTab && tab != ""):
					items = append(items, importItem{URL: subscriptions.ChannelTabURL(id, subscriptions.TabVideos), Title: title})
				case o.HTMLURL != "":
					items = append(items, importItem{URL: strings.TrimSpace(o.HTMLURL), Title: title})
				case o.XMLURL != "":
					items = append(items, importItem{URL: strings.TrimSpace(o.XMLURL), Title: title})
				}
				walk(o.Children)
			}
		}
		walk(doc.Body.Outlines)
	case FormatYouTubeCSV:
		r := csv.NewReader(strings.NewReader(data))
		r.FieldsPerRecord = -1
		header := true
		for {
			rec, err := r.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, invalid("not a CSV file: %v", err)
			}
			if header { // "Channel Id,Channel Url,Channel Title" (localized)
				header = false
				continue
			}
			if len(rec) == 0 {
				continue
			}
			id := strings.TrimSpace(rec[0])
			title := ""
			if len(rec) > 2 {
				title = strings.TrimSpace(rec[2])
			}
			switch {
			case subscriptions.IsChannelID(id):
				items = append(items, importItem{URL: subscriptions.ChannelTabURL(id, subscriptions.TabVideos), Title: title})
			case len(rec) > 1 && strings.TrimSpace(rec[1]) != "":
				items = append(items, importItem{URL: strings.TrimSpace(rec[1]), Title: title})
			}
		}
	default:
		return nil, invalid("format must be urls, opml or youtube-csv")
	}
	return items, nil
}

// ImportSubscriptions creates one manual subscription without backlog per
// channel or playlist of the file (a YouTube channel gets its Videos tab).
// Already subscribed sources are skipped. The first checks are spread over
// one hour, so a large import causes no request burst (§11).
func (m *Manager) ImportSubscriptions(ctx context.Context, format, data string) (*ImportResult, error) {
	if m.shuttingDown() {
		return nil, ErrShuttingDown
	}
	items, err := parseImport(format, data)
	if err != nil {
		return nil, err
	}
	if len(items) > maxImport {
		return nil, invalid("at most %d subscriptions per import", maxImport)
	}
	res := &ImportResult{Created: []SubscriptionView{}, Skipped: []ImportSkip{}}
	type planned struct {
		item importItem
		src  *store.Source
	}
	var plan []planned
	seen := map[string]bool{}
	for _, it := range items {
		u := it.URL
		if config.ValidateURL(u) != nil {
			res.Skipped = append(res.Skipped, ImportSkip{URL: u, Reason: "not a valid URL"})
			continue
		}
		if base, tab, ok := subscriptions.YouTubeChannel(u); ok && tab == "" {
			u = subscriptions.TabURL(base, subscriptions.TabVideos)
		}
		canon, tab := subscriptions.Canonical(u, nil)
		if seen[canon] {
			res.Skipped = append(res.Skipped, ImportSkip{URL: it.URL, Reason: "listed twice"})
			continue
		}
		seen[canon] = true
		plan = append(plan, planned{item: it, src: subscriptions.NewSource(u, canon, tab)})
	}
	var canons []string
	for _, p := range plan {
		canons = append(canons, p.src.Canonical)
	}
	existing, err := m.Store.SourcesByCanonical(ctx, canons)
	if err != nil {
		return nil, err
	}
	taken := map[string]int64{}
	for _, src := range existing {
		taken[src.Canonical] = src.SubscriptionID
	}
	names := m.subNames(ctx)
	now := time.Now()
	n := 0
	for _, p := range plan {
		if id, ok := taken[p.src.Canonical]; ok {
			res.Skipped = append(res.Skipped, ImportSkip{URL: p.item.URL, Reason: "already subscribed",
				SubscriptionID: itoa(id), Name: names[id]})
			continue
		}
		n++
	}
	i := 0
	created := map[int64]bool{}
	var order []int64 // import order
	for _, p := range plan {
		if _, ok := taken[p.src.Canonical]; ok {
			continue
		}
		// An empty name is taken from the first listing.
		sub := &store.Subscription{Name: strings.TrimSpace(p.item.Title), Mode: store.ModeManual, Enabled: true,
			Listing: store.ListingAuto, Sources: []*store.Source{p.src},
			NextCheckAt: now.Add(time.Duration(i) * time.Hour / time.Duration(max(n, 1)))}
		i++
		if err := m.Store.CreateSubscription(ctx, sub); err != nil {
			res.Skipped = append(res.Skipped, ImportSkip{URL: p.item.URL, Reason: m.conflict(ctx, err).Error()})
			continue
		}
		m.subs.logs.Note(sub.ID, "Subscription imported: %s", p.src.URL)
		created[sub.ID] = true
		order = append(order, sub.ID)
	}
	if all, err := m.Store.ListSubscriptions(ctx); err == nil {
		views := map[int64]SubscriptionView{}
		for _, sub := range all {
			if created[sub.ID] {
				views[sub.ID] = m.subView(sub, all)
			}
		}
		for _, id := range order {
			res.Created = append(res.Created, views[id])
			m.Hub.Publish(EventSubscription, views[id])
		}
	}
	m.markCounts(0)
	m.logf("subscriptions: imported %d, skipped %d", len(res.Created), len(res.Skipped))
	m.subs.loop.Wake()
	return res, nil
}

// ExportSubscriptions writes the canonical URL of every source as a URL
// list or as OPML (one outline per source; YouTube channels get their feed
// URL as xmlUrl).
func (m *Manager) ExportSubscriptions(ctx context.Context, format string) ([]byte, error) {
	subs, err := m.Store.ListSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	switch format {
	case "", FormatURLs:
		b.WriteString("# ytdlp-nfo-server subscriptions\n")
		for _, s := range subs {
			for _, src := range s.Sources {
				fmt.Fprintln(&b, src.Canonical)
			}
		}
	case FormatOPML:
		type outline struct {
			XMLName xml.Name `xml:"outline"`
			Text    string   `xml:"text,attr"`
			Title   string   `xml:"title,attr"`
			Type    string   `xml:"type,attr,omitempty"`
			XMLURL  string   `xml:"xmlUrl,attr,omitempty"`
			HTMLURL string   `xml:"htmlUrl,attr"`
		}
		type opml struct {
			XMLName xml.Name  `xml:"opml"`
			Version string    `xml:"version,attr"`
			Title   string    `xml:"head>title"`
			Body    []outline `xml:"body>outline"`
		}
		doc := opml{Version: "2.0", Title: "ytdlp-nfo-server subscriptions"}
		for _, s := range subs {
			for _, src := range s.Sources {
				o := outline{Text: s.Name, Title: s.Name, HTMLURL: src.Canonical}
				if id, ok := strings.CutPrefix(src.Canonical, "https://www.youtube.com/channel/"); ok {
					if id, _, _ = strings.Cut(id, "/"); subscriptions.IsChannelID(id) {
						o.Type, o.XMLURL = "rss", "https://www.youtube.com/feeds/videos.xml?channel_id="+id
					}
				}
				doc.Body = append(doc.Body, o)
			}
		}
		b.WriteString(xml.Header)
		enc := xml.NewEncoder(&b)
		enc.Indent("", "  ")
		if err := enc.Encode(doc); err != nil {
			return nil, err
		}
		b.WriteString("\n")
	default:
		return nil, invalid("format must be urls or opml")
	}
	return b.Bytes(), nil
}
