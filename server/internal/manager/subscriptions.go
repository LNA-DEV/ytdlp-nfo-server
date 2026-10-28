package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/failpoint"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/joblog"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/notify"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/subscriptions"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp/versions"
)

// Event types of subscriptions on the global stream.
const (
	EventSubscription        = "subscription"
	EventSubscriptionRemoved = "subscription_removed"
	EventEntries             = "entries"
)

// subState is the subscription part of the manager.
type subState struct {
	loop   *subscriptions.Loop
	logs   *joblog.Manager
	counts map[int64]store.EntryCounts // cached per subscription (§9)
	dirty  map[int64]bool
	all    bool // recompute every subscription
	signal chan struct{}
}

// initSubscriptions creates the check loop and its log manager.
func (m *Manager) initSubscriptions() error {
	logs, err := joblog.New(filepath.Join(m.dataDir, "logs", "subscriptions"))
	if err != nil {
		return err
	}
	logs.MaxSize, logs.TailOnly = 512<<10, true
	tmp := filepath.Join(m.dataDir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	m.subs = subState{logs: logs, counts: map[int64]store.EntryCounts{}, dirty: map[int64]bool{}, all: true,
		signal: make(chan struct{}, 1)}
	m.subs.loop = subscriptions.NewLoop(subscriptions.Deps{
		Store: m.Store, Host: subHost{m}, Logs: logs, Logf: m.logf,
		CacheDir: filepath.Join(m.dataDir, "ytdlp", "cache"), TmpDir: tmp,
		ListerFor: m.listerFor,
	})
	return nil
}

// startSubscriptions recovers interrupted checks and starts the loops.
func (m *Manager) startSubscriptions() error {
	n, err := m.Store.ResetChecking(m.ctx, time.Now())
	if err != nil {
		return err
	}
	if n > 0 {
		m.logf("recovery: %d interrupted subscription checks run again", n)
	}
	m.bgWG.Add(2)
	go func() { defer m.bgWG.Done(); m.subs.loop.Run(m.ctx) }()
	go m.countsLoop()
	return nil
}

// SubscriptionLogs returns the check log manager.
func (m *Manager) SubscriptionLogs() *joblog.Manager { return m.subs.logs }

// --- subscriptions.Host ---

type subHost struct{ m *Manager }

func (h subHost) Settings() config.Settings { return h.m.Settings.Settings() }

func (h subHost) Resolve(ctx context.Context, sub *store.Subscription) (config.Options, string, error) {
	return h.m.resolveSub(ctx, sub)
}

func (h subHost) Acquire(ctx context.Context, spec string) (versions.Binary, func(), error) {
	return h.m.resolver.Acquire(ctx, spec)
}

func (h subHost) IsLatest(ctx context.Context, b versions.Binary) bool {
	return h.m.resolver.IsLatest(ctx, b)
}

func (h subHost) CopyCookies(profile, path string) error {
	return h.m.copyCookies(context.Background(), profile, path)
}

func (h subHost) Cooldown(site string) time.Time { return h.m.Sched.Status().Cooldowns[site] }

func (h subHost) SetCooldown(site string, until time.Time) { h.m.Sched.SetCooldown(site, until) }

func (h subHost) Commit(ctx context.Context, res *subscriptions.Result) error {
	return h.m.commitCheck(ctx, res)
}

func (h subHost) Changed(id int64) { h.m.publishSub(id) }

// firstURL returns the URL jobs and site rules of a subscription use: its
// first source as entered.
func firstURL(sub *store.Subscription) string {
	if len(sub.Sources) == 0 {
		return ""
	}
	return sub.Sources[0].URL
}

// recheckOptionErrors makes subscriptions whose last check failed on their
// download options due now if the options resolve again, e.g. after a
// library, site rule or cookie profile was restored (§5.2 "options first").
func (m *Manager) recheckOptionErrors(ctx context.Context) {
	if m.subs.loop == nil {
		return
	}
	subs, err := m.Store.ListSubscriptions(ctx)
	if err != nil {
		return
	}
	var ids []int64
	for _, sub := range subs {
		if !sub.Enabled || !subscriptions.IsOptionsError(sub) {
			continue
		}
		if _, _, err := m.resolveSub(ctx, sub); err == nil {
			ids = append(ids, sub.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	if _, err := m.Store.SetNextCheck(ctx, ids, time.Now()); err == nil {
		m.subs.loop.Wake()
	}
}

// resolveSub resolves the download options of a subscription (subscription
// option > site rule > global default) and its scheduler site key. An
// error means the options can't be used, e.g. a removed library.
func (m *Manager) resolveSub(ctx context.Context, sub *store.Subscription) (config.Options, string, error) {
	u := firstURL(sub)
	settings := m.Settings.Settings()
	opts, err := config.Resolve(sub.Requested, &settings, m.siteRules(), u)
	if err != nil {
		return opts, config.SiteKey(config.Host(u)), err
	}
	if opts.CookieProfile != "" {
		if _, err := m.Store.GetCookieProfile(ctx, opts.CookieProfile); err != nil {
			return opts, m.siteGroup(u, opts), fmt.Errorf("unknown cookie profile %q", opts.CookieProfile)
		}
	}
	return opts, m.siteGroup(u, opts), nil
}

func (m *Manager) copyCookies(ctx context.Context, profile, dst string) error {
	prof, err := m.Store.GetCookieProfile(ctx, profile)
	if err != nil {
		return fmt.Errorf("profile %q not found", profile)
	}
	data, err := os.ReadFile(filepath.Join(m.cookieDir, filepath.Base(prof.Filename)))
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

func (m *Manager) statusParams() store.StatusParams {
	days := m.Settings.Settings().Subscriptions.UpcomingDays
	return store.StatusParams{UpcomingSince: time.Now().AddDate(0, 0, -max(days, 1))}
}

// --- commit ---

// commitCheck writes everything a check changes in one transaction: the
// entries, the gone markers, the jobs with their items, the entry links and
// the check state (§5.5). A crash before the commit changes nothing; after
// it, the queued jobs are picked up at startup.
func (m *Manager) commitCheck(ctx context.Context, res *subscriptions.Result) error {
	settings := m.Settings.Settings()
	var (
		out  *subscriptions.Output
		sub  *store.Subscription
		jobs []*store.Job
	)
	// Jobs use the options at commit time. They are resolved before the
	// transaction: no other store call may run while it holds the
	// connection (an in-memory store has only one).
	opts := res.Options
	if cur, err := m.Store.GetSubscription(ctx, res.SubscriptionID); err == nil {
		if o, _, err := m.resolveSub(ctx, cur); err == nil {
			opts = o
		}
	}
	err := m.Store.InTx(ctx, func(tx *store.Tx) error {
		var err error
		// Read again: the subscription may have been edited or deleted
		// during the check; the state at commit time applies.
		if sub, err = tx.GetSubscription(ctx, res.SubscriptionID); err != nil {
			return err
		}
		if err := m.updateCanonicals(ctx, tx, sub, res); err != nil {
			return err
		}
		if res.Full && res.OptionsErr == nil {
			if err := tx.ClearFullRequest(ctx, sub.ID); err != nil {
				return err
			}
		}
		if sub.Name == "" { // imported without a title: named after its first listing
			for _, r := range res.Sources {
				if r.Listing != nil && len(sub.Sources) > 0 && r.SourceID == sub.Sources[0].ID {
					if sub.Name = subscriptions.DisplayName(r.Listing); sub.Name != "" {
						if err := tx.UpdateSubscription(ctx, sub); err != nil {
							return err
						}
					}
				}
			}
		}
		entries, err := tx.EntriesForSubscription(ctx, sub.ID, m.statusParams())
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, e := range entries {
			have[e.Key] = true
			if e.ListingKey != "" {
				have[e.ListingKey] = true
			}
		}
		var keys []string
		for _, r := range res.Sources {
			if r.Listing == nil {
				continue
			}
			for _, le := range r.Listing.Entries {
				if k := subscriptions.EntryKey(le); k != "" && !have[k] {
					keys = append(keys, k)
				}
			}
		}
		archived, err := tx.ArchivedKeys(ctx, keys)
		if err != nil {
			return err
		}
		out = subscriptions.Apply(subscriptions.Input{Now: time.Now(), Sub: sub, Entries: entries, Archived: archived,
			Result: res, Settings: settings.Subscriptions, HasCookies: opts.CookieProfile != ""})
		if res.OptionsErr == nil {
			if len(out.Auto) > 0 {
				j, err := m.subJobTx(ctx, tx, sub, opts, store.BandAuto, out.Auto, archived)
				if err != nil {
					return err
				}
				jobs = append(jobs, j)
			}
			for _, e := range out.Requeue {
				j, err := m.subJobTx(ctx, tx, sub, opts, store.BandAuto, []*store.Entry{e}, archived)
				if err != nil {
					return err
				}
				jobs = append(jobs, j)
			}
			if len(out.Backlog) > 0 {
				j, err := m.subJobTx(ctx, tx, sub, opts, store.BandBacklog, out.Backlog, archived)
				if err != nil {
					return err
				}
				jobs = append(jobs, j)
			}
		}
		for _, e := range out.Insert {
			if err := tx.InsertEntry(ctx, e); err != nil {
				return err
			}
		}
		for _, e := range out.Update {
			if err := tx.UpdateEntry(ctx, e); err != nil {
				return err
			}
		}
		for _, e := range append(append([]*store.Entry(nil), out.Requeue...), out.Backlog...) {
			if !containsEntry(out.Insert, e) && !containsEntry(out.Update, e) {
				if err := tx.UpdateEntry(ctx, e); err != nil {
					return err
				}
			}
		}
		for _, src := range out.Sources {
			if err := tx.SaveSource(ctx, src); err != nil {
				return err
			}
		}
		return tx.SaveCheckState(ctx, sub)
	})
	if err != nil {
		return err
	}
	_ = failpoint.Check("subscription:committed")

	queued := 0
	var ids []int64
	for _, j := range jobs {
		ids = append(ids, j.ID)
		queued += j.Counts.Total
		m.Logs.Note(j.ID, "Job created by subscription %q", sub.Name)
		m.Sched.Enqueue(scheduler.Job{ID: j.ID, Pos: j.QueuePos, Site: j.Site, Band: j.Band})
		m.publishJob(j.ID)
	}
	log := m.subs.logs.Get(sub.ID)
	if res.OptionsErr == nil {
		log.Line(subscriptions.SummaryLine(out.Summary, queued, ids))
	}
	for _, src := range out.Ordered {
		if src.NewestFirst {
			log.Printf("%s lists newest first: later checks list only its newest %d entries", src.Canonical, settings.Subscriptions.Window)
		} else {
			log.Printf("%s lists oldest first: checks keep listing it completely", src.Canonical)
		}
	}
	if sub.Warning != "" && out.CapHit {
		log.Printf("WARNING: %s", sub.Warning)
	}
	if sub.State == store.SubError {
		log.Printf("Next check at %s", sub.NextCheckAt.Format("2006-01-02 15:04"))
	}
	m.subs.logs.Release(sub.ID)
	m.markCounts(sub.ID)
	m.publishSub(sub.ID)
	m.Hub.Publish(EventEntries, map[string]any{"subscription": itoa(sub.ID), "changed": len(out.Insert) + len(out.Update)})
	m.notifyCheck(sub, out)
	return nil
}

// notifyCheck sends the notifications of a check (§11): new entries if the
// subscription has notify set, entering the error state and hitting the
// cap if onFailed is on. Each is sent once, not on every check.
func (m *Manager) notifyCheck(sub *store.Subscription, out *subscriptions.Output) {
	s := m.Settings.Settings().Notifications
	info := notify.SubscriptionInfo{ID: sub.ID, Name: sub.Name}
	type msg struct{ event, title, text string }
	var msgs []msg
	if sub.Notify && out.NewVisible > 0 {
		info.Count = out.NewVisible
		text := fmt.Sprintf("%s: %d new videos", sub.Name, out.NewVisible)
		if out.NewVisible == 1 {
			text = sub.Name + ": 1 new video"
		}
		msgs = append(msgs, msg{notify.EventSubscriptionNew, "New videos", text})
	}
	if s.OnFailed && out.EnteredError {
		msgs = append(msgs, msg{notify.EventSubscriptionError, "Subscription check failed", sub.Name + ": " + sub.Error})
	}
	if s.OnFailed && out.CapHit {
		msgs = append(msgs, msg{notify.EventSubscriptionCap, "Subscription needs review", sub.Name + ": " + sub.Warning})
	}
	if len(msgs) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, x := range msgs {
			m.Notifier.SendSubscription(ctx, s, x.event, x.title, x.text, info)
		}
	}()
}

func containsEntry(es []*store.Entry, e *store.Entry) bool {
	for _, x := range es {
		if x == e {
			return true
		}
	}
	return false
}

// updateCanonicals replaces the provisional canonical URL of a source added
// without a preview (a handle URL from an import or the API) once its
// listing tells the channel ID. A source that then turns out to be
// subscribed already is removed again, with a warning (§11).
func (m *Manager) updateCanonicals(ctx context.Context, tx *store.Tx, sub *store.Subscription, res *subscriptions.Result) error {
	byID := map[int64]*store.Source{}
	for _, src := range sub.Sources {
		byID[src.ID] = src
	}
	for i := range res.Sources {
		r := &res.Sources[i]
		src := byID[r.SourceID]
		if src == nil || r.Listing == nil {
			continue
		}
		canon, _ := subscriptions.Canonical(src.URL, r.Listing)
		if canon == src.Canonical {
			continue
		}
		others, err := tx.SourcesByCanonical(ctx, []string{canon})
		if err != nil {
			return err
		}
		if len(others) > 0 && others[0].ID != src.ID {
			if err := tx.RemoveSource(ctx, sub.ID, src.ID); err != nil {
				return err
			}
			r.Listing, r.Err = nil, nil
			r.SourceID = 0
			sub.Sources = removeSource(sub.Sources, src.ID)
			name := fmt.Sprintf("subscription %d", others[0].SubscriptionID)
			if o, err := tx.GetSubscription(ctx, others[0].SubscriptionID); err == nil {
				name = fmt.Sprintf("%q", o.Name)
			}
			sub.Warning = fmt.Sprintf("%s is already subscribed in %s; the source was removed.", src.URL, name)
			if len(sub.Sources) == 0 {
				// Nothing left to check (an imported duplicate): pause it
				// until the user adds a source or deletes it.
				sub.Enabled = false
				if sub.Name == "" {
					sub.Name = src.URL
				}
				sub.Warning += " Delete this subscription or add another source."
			}
			if err := tx.UpdateSubscription(ctx, sub); err != nil {
				return err
			}
			continue
		}
		src.Canonical = canon
		if err := tx.SaveSource(ctx, src); err != nil {
			return err
		}
	}
	return nil
}

func removeSource(srcs []*store.Source, id int64) []*store.Source {
	var out []*store.Source
	for _, s := range srcs {
		if s.ID != id {
			out = append(out, s)
		}
	}
	return out
}

// subJobTx creates one job with pre-planned items for entries of a
// subscription and links the entries to the items (§4.3). Items run oldest
// first; items whose key is in the archive are created as skipped.
func (m *Manager) subJobTx(ctx context.Context, tx *store.Tx, sub *store.Subscription, opts config.Options, band store.Band,
	entries []*store.Entry, archived map[string]bool) (*store.Job, error) {
	srcByID := map[int64]*store.Source{}
	for _, src := range sub.Sources {
		srcByID[src.ID] = src
	}
	entries = append([]*store.Entry(nil), entries...)
	subscriptions.SortOldestFirst(entries, srcByID)
	u := firstURL(sub)
	j := &store.Job{URL: u, Site: m.siteGroup(u, opts), State: store.JobQueued, Requested: sub.Requested, Options: opts,
		SubscriptionID: sub.ID, Band: band, Title: subJobTitle(sub.Name, band, entries), Uploader: sub.Uploader,
		Thumbnail: sub.Thumbnail, IsPlaylist: len(entries) > 1}
	if len(entries) == 1 && entries[0].Thumbnail != "" {
		j.Thumbnail = entries[0].Thumbnail
	}
	j.Options.RedditProviders = sub.Provider
	if err := tx.CreateJob(ctx, j); err != nil {
		return nil, err
	}
	folder := sub.EffectiveFolder()
	items := make([]*store.Item, len(entries))
	now := time.Now()
	for i, e := range entries {
		it := &store.Item{JobID: j.ID, Index: i, URL: e.URL, Title: e.Title, PlaylistTitle: folder, State: store.ItemPending}
		if !strings.HasPrefix(e.Key, "url ") {
			it.ArchiveKey = e.Key
			it.Extractor, it.VideoID, _ = strings.Cut(e.Key, " ")
		}
		if it.Title == "" {
			it.Title = e.URL
		}
		if it.ArchiveKey != "" && !opts.Force && (e.InArchive || archived[e.Key]) {
			it.State, it.Error, it.FinishedAt = store.ItemSkipped, "Already in library", now
		}
		items[i] = it
	}
	if err := tx.CreateItems(ctx, items); err != nil {
		return nil, err
	}
	for i, e := range entries {
		e.ItemID = items[i].ID
		e.ItemState = items[i].State
	}
	j.Counts.Total = len(items)
	return j, nil
}

// subJobTitle names a subscription job (§4.3).
func subJobTitle(name string, band store.Band, entries []*store.Entry) string {
	if len(entries) == 1 {
		t := entries[0].Title
		if t == "" {
			t = entries[0].URL
		}
		return name + ": " + t
	}
	switch band {
	case store.BandAuto:
		return fmt.Sprintf("%s: %d new videos", name, len(entries))
	case store.BandBacklog:
		return fmt.Sprintf("%s: %d videos from the backlog", name, len(entries))
	}
	return fmt.Sprintf("%s: %d videos", name, len(entries))
}

// --- counts cache ---

// markCounts schedules a recount of a subscription's entries (0 = all).
func (m *Manager) markCounts(id int64) {
	m.mu.Lock()
	if id == 0 {
		m.subs.all = true
	} else {
		m.subs.dirty[id] = true
	}
	m.mu.Unlock()
	select {
	case m.subs.signal <- struct{}{}:
	default:
	}
}

// countsLoop recomputes dirty counts, debounced, and publishes them.
func (m *Manager) countsLoop() {
	defer m.bgWG.Done()
	for {
		m.recount()
		select {
		case <-m.ctx.Done():
			return
		case <-m.subs.signal:
		}
		select { // coalesce bursts
		case <-m.ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func (m *Manager) recount() {
	m.mu.Lock()
	all, dirty := m.subs.all, m.subs.dirty
	m.subs.all, m.subs.dirty = false, map[int64]bool{}
	m.mu.Unlock()
	if !all && len(dirty) == 0 {
		return
	}
	p := m.statusParams()
	var ids []int64
	if all {
		counts, err := m.Store.CountEntries(m.ctx, 0, p)
		if err != nil {
			return
		}
		m.mu.Lock()
		m.subs.counts = counts
		m.mu.Unlock()
		subs, _ := m.Store.ListSubscriptions(m.ctx)
		for _, s := range subs {
			ids = append(ids, s.ID)
		}
	} else {
		for id := range dirty {
			c, err := m.Store.CountEntries(m.ctx, id, p)
			if err != nil {
				continue
			}
			m.mu.Lock()
			m.subs.counts[id] = c[id]
			m.mu.Unlock()
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		m.publishSub(id)
	}
	m.publishCounts()
}

func (m *Manager) subCounts(id int64) store.EntryCounts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subs.counts[id]
}

// Inbox returns the number of new entries over all subscriptions.
func (m *Manager) Inbox() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.subs.counts {
		n += c.New
	}
	return n
}

// itemFinished marks the counts of an item's subscription when the item
// reaches a terminal state.
func (m *Manager) itemFinished(it *store.Item) {
	if sub, err := m.Store.SubscriptionForItem(context.Background(), it.ID); err == nil && sub != 0 {
		m.markCounts(sub)
		m.Hub.Publish(EventEntries, map[string]any{"subscription": itoa(sub), "changed": 1})
	}
}

// publishSub publishes the current view of a subscription.
func (m *Manager) publishSub(id int64) {
	sub, err := m.Store.GetSubscription(context.Background(), id)
	if err != nil {
		return
	}
	m.Hub.Publish(EventSubscription, m.subView(sub, nil))
}

var errNoSources = errors.New("a subscription needs at least one source")
