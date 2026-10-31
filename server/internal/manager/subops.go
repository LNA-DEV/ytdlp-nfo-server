package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/failpoint"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/library"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/scheduler"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/store"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/subscriptions"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// ErrUnprocessable is returned when a URL cannot be subscribed to (a video
// without a channel, or not a listing at all).
var ErrUnprocessable = errors.New("cannot subscribe")

// SubscriptionConflict is returned when a source is already subscribed.
type SubscriptionConflict struct {
	URL            string
	SubscriptionID int64
	Name           string
}

func (e *SubscriptionConflict) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("%s is already subscribed in %q", e.URL, e.Name)
	}
	return e.URL + " is already subscribed"
}

// --- views ---

// SourceView is the API representation of a source.
type SourceView struct {
	ID          string     `json:"id"`
	URL         string     `json:"url"`
	Canonical   string     `json:"canonical"`
	Tab         string     `json:"tab,omitempty"`
	AddedAt     time.Time  `json:"addedAt"`
	FirstOKAt   *time.Time `json:"firstOkAt,omitempty"`
	LastOKAt    *time.Time `json:"lastOkAt,omitempty"`
	LastFullAt  *time.Time `json:"lastFullAt,omitempty"`
	Impersonate bool       `json:"impersonate,omitempty"`
	NewestFirst bool       `json:"newestFirst"`
	OrderKnown  bool       `json:"orderKnown"`
	Error       string     `json:"error,omitempty"`
	ErrorClass  string     `json:"errorClass,omitempty"`
}

// SubscriptionView is the API representation of a subscription.
type SubscriptionView struct {
	ID                string                `json:"id"`
	Name              string                `json:"name"`
	Folder            string                `json:"folder"`
	EffectiveFolder   string                `json:"effectiveFolder"`
	Kind              string                `json:"kind"`
	Uploader          string                `json:"uploader"`
	Thumbnail         string                `json:"thumbnail"`
	Mode              string                `json:"mode"`
	Enabled           bool                  `json:"enabled"`
	Backlog           store.BacklogChoice   `json:"backlog"`
	Filters           store.Filters         `json:"filters"`
	Options           config.OptionsRequest `json:"options"`
	Library           string                `json:"library"`
	IntervalMinutes   int                   `json:"intervalMinutes"`
	EffectiveInterval int                   `json:"effectiveIntervalMinutes"`
	Listing           string                `json:"listing"`
	Notify            bool                  `json:"notify"`
	State             string                `json:"state"`
	Failures          int                   `json:"failures"`
	Error             string                `json:"error,omitempty"`
	ErrorClass        string                `json:"errorClass,omitempty"`
	Hint              string                `json:"hint,omitempty"`
	Warning           string                `json:"warning,omitempty"`
	LastCheckAt       *time.Time            `json:"lastCheckAt,omitempty"`
	LastOKAt          *time.Time            `json:"lastOkAt,omitempty"`
	NextCheckAt       *time.Time            `json:"nextCheckAt,omitempty"`
	CreatedAt         time.Time             `json:"createdAt"`
	Sources           []SourceView          `json:"sources"`
	Counts            store.EntryCounts     `json:"counts"`
	SharedWith        []string              `json:"sharedWith,omitempty"` // subscriptions using the same folder
}

func (m *Manager) subView(sub *store.Subscription, all []*store.Subscription) SubscriptionView {
	settings := m.Settings.Settings()
	lib := m.subLibrary(sub, settings)
	v := SubscriptionView{
		ID: itoa(sub.ID), Name: sub.Name, Folder: sub.Folder, EffectiveFolder: sub.EffectiveFolder(), Kind: sub.Kind,
		Uploader: sub.Uploader, Thumbnail: sub.Thumbnail, Mode: sub.Mode, Enabled: sub.Enabled, Backlog: sub.Backlog,
		Filters: sub.Filters, Options: sub.Requested, Library: lib, IntervalMinutes: sub.IntervalMinutes,
		EffectiveInterval: int(subscriptions.Interval(sub, settings.Subscriptions) / time.Minute), Listing: sub.Listing,
		Notify: sub.Notify, State: sub.State, Failures: sub.Failures, Error: sub.Error, ErrorClass: sub.ErrorClass,
		Hint: sub.Hint, Warning: sub.Warning, LastCheckAt: tptr(sub.LastCheckAt), LastOKAt: tptr(sub.LastOKAt),
		NextCheckAt: tptr(sub.NextCheckAt), CreatedAt: sub.CreatedAt, Sources: []SourceView{}, Counts: m.subCounts(sub.ID),
	}
	if v.Name == "" {
		v.Name = firstURL(sub) // imported, not listed yet
	}
	if m.subs.loop.Running(sub.ID) {
		v.State = store.SubChecking // until the loop has released it, a Check now would be ignored
	}
	for _, src := range sub.Sources {
		v.Sources = append(v.Sources, SourceView{ID: itoa(src.ID), URL: src.URL, Canonical: src.Canonical, Tab: src.Tab,
			AddedAt: src.AddedAt, FirstOKAt: tptr(src.FirstOKAt), LastOKAt: tptr(src.LastOKAt), LastFullAt: tptr(src.LastFullAt),
			Impersonate: src.Impersonate, NewestFirst: src.NewestFirst, OrderKnown: src.OrderKnown, Error: src.Error,
			ErrorClass: src.ErrorClass})
	}
	if all == nil {
		all, _ = m.Store.ListSubscriptions(context.Background())
	}
	folder := library.SanitizeFilename(strings.TrimSpace(sub.EffectiveFolder()))
	for _, o := range all {
		if o.ID != sub.ID && library.SanitizeFilename(strings.TrimSpace(o.EffectiveFolder())) == folder && m.subLibrary(o, settings) == lib {
			v.SharedWith = append(v.SharedWith, o.Name)
		}
	}
	return v
}

// subLibrary returns the effective library name of a subscription.
func (m *Manager) subLibrary(sub *store.Subscription, settings config.Settings) string {
	if sub.Requested.Library != nil && *sub.Requested.Library != "" {
		return *sub.Requested.Library
	}
	if r := config.MatchRule(m.siteRules(), config.Host(firstURL(sub))); r != nil && r.Library != "" {
		return r.Library
	}
	return settings.Defaults.Library
}

// EntryView is the API representation of an entry with its derived status.
type EntryView struct {
	ID             string     `json:"id"`
	SubscriptionID string     `json:"subscriptionId"`
	Subscription   string     `json:"subscription,omitempty"`
	Key            string     `json:"key"`
	URL            string     `json:"url"`
	Title          string     `json:"title"`
	Duration       float64    `json:"duration,omitempty"`
	UploadDate     string     `json:"uploadDate,omitempty"`
	SortDate       time.Time  `json:"sortDate"`
	Thumbnail      string     `json:"thumbnail,omitempty"`
	LiveStatus     string     `json:"liveStatus,omitempty"`
	Availability   string     `json:"availability,omitempty"`
	SourceID       string     `json:"sourceId,omitempty"`
	Backlog        bool       `json:"backlog"`
	FirstSeenAt    time.Time  `json:"firstSeenAt"`
	GoneAt         *time.Time `json:"goneAt,omitempty"`
	Ignored        bool       `json:"ignored"`
	IgnoreReason   string     `json:"ignoreReason,omitempty"`
	Status         string     `json:"status"`
	JobID          string     `json:"jobId,omitempty"`
	ItemID         string     `json:"itemId,omitempty"`
	ItemState      string     `json:"itemState,omitempty"`
	Error          string     `json:"error,omitempty"`
	Hint           string     `json:"hint,omitempty"`
}

func entryView(e *store.Entry, names map[int64]string) EntryView {
	v := EntryView{ID: itoa(e.ID), SubscriptionID: itoa(e.SubscriptionID), Subscription: names[e.SubscriptionID],
		Key: e.Key, URL: e.URL, Title: e.Title, Duration: e.Duration, UploadDate: e.UploadDate, SortDate: e.SortDate,
		Thumbnail: e.Thumbnail, LiveStatus: e.LiveStatus, Availability: e.Availability, Backlog: e.Backlog,
		FirstSeenAt: e.FirstSeenAt, GoneAt: tptr(e.GoneAt), Ignored: e.Ignored, IgnoreReason: e.IgnoreReason,
		Status: e.Status, ItemState: string(e.ItemState)}
	if e.SourceID != 0 {
		v.SourceID = itoa(e.SourceID)
	}
	if e.ItemID != 0 {
		v.ItemID = itoa(e.ItemID)
		if e.JobID != 0 {
			v.JobID = itoa(e.JobID)
		}
		if e.ItemState == store.ItemFailed || e.ItemState == store.ItemCancelled {
			v.Error, v.Hint = e.ItemError, e.ItemHint
		}
	}
	return v
}

// --- list and get ---

// ListSubscriptions returns all subscriptions (filtered by q on name,
// uploader and source URLs).
func (m *Manager) ListSubscriptions(ctx context.Context, q string) ([]SubscriptionView, error) {
	subs, err := m.Store.ListSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	q = strings.ToLower(strings.TrimSpace(q))
	out := []SubscriptionView{}
	for _, sub := range subs {
		if q != "" && !subMatches(sub, q) {
			continue
		}
		out = append(out, m.subView(sub, subs))
	}
	return out, nil
}

func subMatches(sub *store.Subscription, q string) bool {
	if strings.Contains(strings.ToLower(sub.Name), q) || strings.Contains(strings.ToLower(sub.Uploader), q) ||
		strings.Contains(strings.ToLower(sub.Folder), q) {
		return true
	}
	for _, src := range sub.Sources {
		if strings.Contains(strings.ToLower(src.URL), q) || strings.Contains(strings.ToLower(src.Canonical), q) {
			return true
		}
	}
	return false
}

// GetSubscription returns one subscription.
func (m *Manager) GetSubscription(ctx context.Context, id int64) (*SubscriptionView, error) {
	sub, err := m.Store.GetSubscription(ctx, id)
	if err != nil {
		return nil, err
	}
	v := m.subView(sub, nil)
	return &v, nil
}

// SubscriptionNames maps subscription ids to names.
func (m *Manager) subNames(ctx context.Context) map[int64]string {
	subs, _ := m.Store.ListSubscriptions(ctx)
	out := map[int64]string{}
	for _, s := range subs {
		out[s.ID] = s.Name
	}
	return out
}

// EntryListFilter selects entries (subscription 0 = the feed).
type EntryListFilter = store.EntryFilter

// ListEntries returns one page of entries with their derived status.
func (m *Manager) ListEntries(ctx context.Context, f EntryListFilter) ([]EntryView, error) {
	if !store.ValidEntryStatus(f.Status) {
		return nil, fmt.Errorf("%w: unknown status %q", ErrInvalid, f.Status)
	}
	if f.SubscriptionID != 0 {
		if _, err := m.Store.GetSubscription(ctx, f.SubscriptionID); err != nil {
			return nil, err
		}
	}
	es, err := m.Store.ListEntries(ctx, f, m.statusParams())
	if err != nil {
		return nil, err
	}
	names := m.subNames(ctx)
	out := make([]EntryView, 0, len(es))
	for _, e := range es {
		out = append(out, entryView(e, names))
	}
	return out, nil
}

// --- create, update, delete ---

// SourceInput is a source in a create or update request. Canonical is the
// canonical URL from the preview (optional).
type SourceInput struct {
	URL       string `json:"url"`
	Canonical string `json:"canonical,omitempty"`
}

// SubscriptionInput creates (POST) or changes (PATCH) a subscription. Nil
// fields are not changed.
type SubscriptionInput struct {
	Name            *string                `json:"name,omitempty"`
	Folder          *string                `json:"folder,omitempty"`
	Sources         []SourceInput          `json:"sources,omitempty"`
	AddSources      []SourceInput          `json:"addSources,omitempty"`
	RemoveSources   []string               `json:"removeSources,omitempty"`
	Mode            *string                `json:"mode,omitempty"`
	Enabled         *bool                  `json:"enabled,omitempty"`
	Backlog         *store.BacklogChoice   `json:"backlog,omitempty"`
	Filters         *store.Filters         `json:"filters,omitempty"`
	Options         *config.OptionsRequest `json:"options,omitempty"`
	IntervalMinutes *int                   `json:"intervalMinutes,omitempty"`
	Listing         *string                `json:"listing,omitempty"`
	Notify          *bool                  `json:"notify,omitempty"`
	DismissWarning  bool                   `json:"dismissWarning,omitempty"`
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// sourcesFromInput validates sources and computes their canonical URL. A
// YouTube channel root stands for its Videos tab, as in the add dialog. A
// canonical URL from the preview is accepted if it is the one the URL
// itself gives, or the channel-ID form of the same tab.
func sourcesFromInput(in []SourceInput) ([]*store.Source, error) {
	var out []*store.Source
	seen := map[string]bool{}
	for _, s := range in {
		u := strings.TrimSpace(s.URL)
		if err := config.ValidateURL(u); err != nil {
			return nil, invalid("source %q: %v", s.URL, err)
		}
		if base, tab, ok := subscriptions.YouTubeChannel(u); ok && tab == "" {
			u = subscriptions.TabURL(base, subscriptions.TabVideos)
		}
		canon, tab := subscriptions.Canonical(u, nil)
		if c := strings.TrimSpace(s.Canonical); c != "" && c != canon {
			id, ok := strings.CutPrefix(c, "https://www.youtube.com/channel/")
			id, ctab, _ := strings.Cut(id, "/")
			if !ok || tab == "" || ctab != tab || !subscriptions.IsChannelID(id) {
				return nil, invalid("source %q: canonical URL %q does not match", s.URL, c)
			}
			canon = c
		}
		if seen[canon] {
			return nil, invalid("source %q is listed twice", s.URL)
		}
		seen[canon] = true
		out = append(out, &store.Source{URL: u, Canonical: canon, Tab: tab})
	}
	return out, nil
}

// applyInput validates a request against a subscription (new or stored)
// and applies it, including that its download options resolve.
func (m *Manager) applyInput(ctx context.Context, sub *store.Subscription, in SubscriptionInput, creating bool) (optionsChanged, filtersChanged bool, err error) {
	if optionsChanged, filtersChanged, err = assignInput(sub, in, creating); err != nil {
		return false, false, err
	}
	if creating || optionsChanged {
		if len(sub.Sources) == 0 {
			return false, false, invalid("%v", errNoSources)
		}
		if _, _, err := m.resolveSub(ctx, sub); err != nil {
			return false, false, invalid("%v", err)
		}
	}
	return optionsChanged, filtersChanged, nil
}

// assignInput applies the fields of a request to a subscription and
// validates their values. It makes no store calls, so it can run inside a
// transaction.
func assignInput(sub *store.Subscription, in SubscriptionInput, creating bool) (optionsChanged, filtersChanged bool, err error) {
	if in.Name != nil {
		sub.Name = strings.TrimSpace(*in.Name)
	}
	// A subscription imported without a title has no name until its first
	// listing names it; changing another field keeps it that way.
	if sub.Name == "" && (creating || in.Name != nil) || len(sub.Name) > 200 {
		return false, false, invalid("name is required (at most 200 characters)")
	}
	if in.Folder != nil {
		sub.Folder = strings.TrimSpace(*in.Folder)
		if len(sub.Folder) > 200 {
			return false, false, invalid("folder must be at most 200 characters")
		}
	}
	if in.Mode != nil {
		if *in.Mode != store.ModeAuto && *in.Mode != store.ModeManual {
			return false, false, invalid("mode must be auto or manual")
		}
		sub.Mode = *in.Mode
	}
	if in.Enabled != nil {
		sub.Enabled = *in.Enabled
	}
	if in.Backlog != nil {
		b := *in.Backlog
		switch {
		case b.Mode == "" || b.Mode == store.BacklogNone:
			b = store.BacklogChoice{}
		case b.Mode == store.BacklogAll:
			b.N = 0
		case b.Mode == store.BacklogLatest && b.N >= 1 && b.N <= 100000:
		default:
			return false, false, invalid("backlog must be none, all or latest with n ≥ 1")
		}
		if !creating && b.Pending() {
			return false, false, invalid("the backlog choice is made when subscribing; use Download backlog")
		}
		sub.Backlog = b
	}
	if in.Filters != nil {
		if _, err := subscriptions.CompileFilters(*in.Filters); err != nil {
			return false, false, invalid("filters: %v", err)
		}
		filtersChanged = *in.Filters != sub.Filters
		sub.Filters = *in.Filters
	}
	if in.Options != nil {
		optionsChanged = true
		sub.Requested = *in.Options
	}
	if in.IntervalMinutes != nil {
		n := *in.IntervalMinutes
		if n != 0 && (n < config.MinIntervalMinutes || n > config.MaxIntervalMinutes) {
			return false, false, invalid("intervalMinutes must be 0 (default) or between %d and %d", config.MinIntervalMinutes, config.MaxIntervalMinutes)
		}
		sub.IntervalMinutes = n
	}
	if in.Listing != nil {
		switch *in.Listing {
		case store.ListingAuto, store.ListingWindow, store.ListingFull:
			sub.Listing = *in.Listing
		default:
			return false, false, invalid("listing must be auto, window or full")
		}
	}
	if in.Notify != nil {
		sub.Notify = *in.Notify
	}
	if in.DismissWarning {
		sub.Warning = ""
	}
	return optionsChanged, filtersChanged, nil
}

func (m *Manager) conflict(ctx context.Context, err error) error {
	var c *store.SourceConflictError
	if !errors.As(err, &c) {
		return err
	}
	out := &SubscriptionConflict{URL: c.Canonical, SubscriptionID: c.SubscriptionID}
	if sub, err := m.Store.GetSubscription(ctx, c.SubscriptionID); err == nil {
		out.Name = sub.Name
	}
	return out
}

// CreateSubscription stores a subscription. Its first check runs right
// away and fills the backlog (§6).
func (m *Manager) CreateSubscription(ctx context.Context, in SubscriptionInput) (*SubscriptionView, error) {
	if m.shuttingDown() {
		return nil, ErrShuttingDown
	}
	srcs, err := sourcesFromInput(in.Sources)
	if err != nil {
		return nil, err
	}
	if len(srcs) == 0 || len(srcs) > 10 {
		return nil, invalid("a subscription needs 1 to 10 sources")
	}
	sub := &store.Subscription{Mode: store.ModeManual, Enabled: true, Listing: store.ListingAuto, Sources: srcs,
		NextCheckAt: time.Now()}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		name := srcs[0].URL
		in.Name = &name
	}
	if _, _, err := m.applyInput(ctx, sub, in, true); err != nil {
		return nil, err
	}
	if err := m.Store.CreateSubscription(ctx, sub); err != nil {
		return nil, m.conflict(ctx, err)
	}
	m.subs.logs.Note(sub.ID, "Subscription created: %s (%d source(s), mode %s)", sub.Name, len(srcs), sub.Mode)
	m.markCounts(sub.ID)
	m.publishSub(sub.ID)
	m.subs.loop.Wake()
	return m.GetSubscription(ctx, sub.ID)
}

// UpdateSubscription changes a subscription. New sources start with a
// first listing that records their backlog; changed filters re-evaluate
// the new, backlog and filter-ignored entries (§4.1).
func (m *Manager) UpdateSubscription(ctx context.Context, id int64, in SubscriptionInput) (*SubscriptionView, error) {
	sub, err := m.Store.GetSubscription(ctx, id)
	if err != nil {
		return nil, err
	}
	added, err := sourcesFromInput(in.AddSources)
	if err != nil {
		return nil, err
	}
	removed := map[int64]bool{}
	for _, s := range in.RemoveSources {
		sid, _ := strconv.ParseInt(s, 10, 64)
		removed[sid] = true
	}
	var keep []*store.Source
	for _, src := range sub.Sources {
		if !removed[src.ID] {
			keep = append(keep, src)
		}
	}
	sub.Sources = append(keep, added...)
	if len(sub.Sources) == 0 || len(sub.Sources) > 10 {
		return nil, invalid("a subscription needs 1 to 10 sources")
	}
	// Validated here: resolving the options needs store calls, which can't
	// run inside the transaction.
	optionsChanged, _, err := m.applyInput(ctx, sub, in, false)
	if err != nil {
		return nil, err
	}
	_ = failpoint.Check("subscription:edit")
	now := time.Now()
	settings := m.Settings.Settings().Subscriptions
	p := m.statusParams()
	filtersChanged := false
	err = m.Store.InTx(ctx, func(tx *store.Tx) error {
		cur, err := tx.GetSubscription(ctx, id)
		if err != nil {
			return err
		}
		// A check may have committed since sub was read: the request is
		// applied to the current state, so the backlog choice, warning,
		// next check and name it recorded are not overwritten (§5.5).
		oldInterval := cur.IntervalMinutes
		if _, filtersChanged, err = assignInput(cur, in, false); err != nil {
			return err
		}
		if optionsChanged || len(added) > 0 {
			cur.NextCheckAt = now // options first: a fixed option makes the next check succeed
		}
		if cur.IntervalMinutes != oldInterval {
			if next := now.Add(subscriptions.Interval(cur, settings)); cur.NextCheckAt.After(next) {
				cur.NextCheckAt = next
			}
		}
		for _, src := range cur.Sources {
			if removed[src.ID] {
				if err := tx.RemoveSource(ctx, id, src.ID); err != nil {
					return err
				}
			}
		}
		if len(added) > 0 {
			if err := tx.AddSources(ctx, id, added); err != nil {
				return err
			}
		}
		if optionsChanged {
			if err := tx.ResetImpersonation(ctx, id); err != nil {
				return err
			}
		}
		if filtersChanged {
			if err := refilterTx(ctx, tx, cur, p); err != nil {
				return err
			}
		}
		return tx.UpdateSubscription(ctx, cur)
	})
	if err != nil {
		return nil, m.conflict(ctx, err)
	}
	m.refreshCounts(ctx, id)
	m.markCounts(id)
	m.publishSub(id)
	if filtersChanged {
		m.Hub.Publish(EventEntries, map[string]any{"subscription": itoa(id), "changed": 1})
	}
	m.subs.loop.Wake()
	return m.GetSubscription(ctx, id)
}

// refilterTx evaluates changed filters again: new and backlog entries that
// now fail get the new reason, filter-ignored entries that now pass go back
// to new or backlog. Entries ignored by the user are not touched, and
// nothing is queued (S3).
func refilterTx(ctx context.Context, tx *store.Tx, sub *store.Subscription, p store.StatusParams) error {
	f, err := subscriptions.CompileFilters(sub.Filters)
	if err != nil {
		return err
	}
	entries, err := tx.EntriesForSubscription(ctx, sub.ID, p)
	if err != nil {
		return err
	}
	for _, e := range entries {
		open := !e.Ignored && (e.Status == store.EntryNew || e.Status == store.EntryBacklog)
		byFilter := e.Ignored && subscriptions.IsFilterReason(e.IgnoreReason)
		if !open && !byFilter {
			continue
		}
		reason := f.Reason(e)
		switch {
		case reason != "" && reason != e.IgnoreReason:
			e.Ignored, e.IgnoreReason = true, reason
		case reason == "" && byFilter:
			e.Ignored, e.IgnoreReason = false, ""
		default:
			continue
		}
		if err := tx.UpdateEntry(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// DeleteSubscription removes a subscription and its entries. With
// cancelQueued its queued and running jobs are cancelled first; the
// library, the archive and finished jobs stay.
func (m *Manager) DeleteSubscription(ctx context.Context, id int64, cancelQueued bool) error {
	if _, err := m.Store.GetSubscription(ctx, id); err != nil {
		return err
	}
	if cancelQueued {
		ids, err := m.Store.SubscriptionJobIDs(ctx, id)
		if err != nil {
			return err
		}
		for _, jid := range ids {
			if _, err := m.Cancel(ctx, jid); err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}
	if err := m.Store.DeleteSubscription(ctx, id); err != nil {
		return err
	}
	m.subs.logs.Delete(id)
	m.mu.Lock()
	delete(m.subs.counts, id)
	m.mu.Unlock()
	m.Hub.Publish(EventSubscriptionRemoved, map[string]string{"id": itoa(id)})
	m.publishCounts()
	return nil
}

// --- checks ---

// CheckSubscription makes a subscription due now (Check now); full lists
// every source completely. A subscription is never checked twice at the
// same time: during the listing of a running check the request is absorbed
// by that check (its commit schedules the next one), after its commit it
// starts a new check once the loop has released the running one.
func (m *Manager) CheckSubscription(ctx context.Context, id int64, full bool) error {
	sub, err := m.Store.GetSubscription(ctx, id)
	if err != nil {
		return err
	}
	if !sub.Enabled {
		return fmt.Errorf("%w: the subscription is paused", ErrConflict)
	}
	if full {
		// Stored, so it survives a restart. A check that is running now
		// leaves it for the next one.
		err = m.Store.RequestFullCheck(ctx, id, time.Now())
	} else {
		_, err = m.Store.SetNextCheck(ctx, []int64{id}, time.Now())
	}
	if err != nil {
		return err
	}
	m.publishSub(id)
	m.subs.loop.Wake()
	return nil
}

// CheckSubscriptions makes the given (or all) enabled subscriptions due
// now (Check all, external cron).
func (m *Manager) CheckSubscriptions(ctx context.Context, ids []int64) (int, error) {
	n, err := m.Store.SetNextCheck(ctx, ids, time.Now())
	if err != nil {
		return 0, err
	}
	m.subs.loop.Wake()
	return n, nil
}

// --- entries ---

// DownloadEntriesRequest selects entries to download: ids (interactive),
// or all new entries (auto band) or the backlog, optionally the latest N
// (backlog band) of one subscription.
type DownloadEntriesRequest struct {
	IDs          []string `json:"ids,omitempty"`
	Subscription string   `json:"subscription,omitempty"`
	Status       string   `json:"status,omitempty"` // new | backlog
	Latest       int      `json:"latest,omitempty"`
	Force        bool     `json:"force,omitempty"` // re-download entries in the library
}

// DownloadEntriesResult lists the created jobs.
type DownloadEntriesResult struct {
	Jobs    []JobView `json:"jobs"`
	Queued  int       `json:"queued"`
	Skipped int       `json:"skipped"` // already queued, or in the library without force
}

// DownloadEntries creates one job per subscription for the selected
// entries. Entries with an active item are skipped inside the transaction,
// so a double click creates one job (§5.5).
func (m *Manager) DownloadEntries(ctx context.Context, req DownloadEntriesRequest) (*DownloadEntriesResult, error) {
	if m.shuttingDown() {
		return nil, ErrShuttingDown
	}
	var ids []int64
	for _, s := range req.IDs {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, invalid("invalid entry id %q", s)
		}
		ids = append(ids, id)
	}
	band := store.BandInteractive
	var subID int64
	if len(ids) == 0 {
		var err error
		if subID, err = strconv.ParseInt(req.Subscription, 10, 64); err != nil {
			return nil, invalid("ids or subscription is required")
		}
		switch req.Status {
		case store.EntryNew:
			band = store.BandAuto
		case store.EntryBacklog:
			band = store.BandBacklog
		default:
			return nil, invalid("status must be new or backlog")
		}
		if req.Latest < 0 {
			return nil, invalid("latest must not be negative")
		}
	}
	p := m.statusParams()
	// Resolve the options of the affected subscriptions before the
	// transaction (no store calls may run inside it).
	subIDs := map[int64]bool{}
	if subID != 0 {
		subIDs[subID] = true
	} else {
		es, err := m.Store.EntriesByID(ctx, ids, p)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			subIDs[e.SubscriptionID] = true
		}
	}
	subs := map[int64]*store.Subscription{}
	opts := map[int64]config.Options{}
	for id := range subIDs {
		sub, err := m.Store.GetSubscription(ctx, id)
		if err != nil {
			return nil, err
		}
		if req.Force {
			t := true
			sub.Requested.Force = &t
		}
		o, _, err := m.resolveSub(ctx, sub)
		if err != nil {
			return nil, invalid("%s: %v", sub.Name, err)
		}
		subs[id], opts[id] = sub, o
	}
	res := &DownloadEntriesResult{Jobs: []JobView{}}
	var jobs []*store.Job
	err := m.Store.InTx(ctx, func(tx *store.Tx) error {
		var entries []*store.Entry
		var err error
		if subID != 0 {
			if entries, err = tx.EntriesForSubscription(ctx, subID, p); err != nil {
				return err
			}
			var sel []*store.Entry
			for _, e := range entries {
				if e.Status == req.Status {
					sel = append(sel, e)
				}
			}
			if req.Latest > 0 && len(sel) > req.Latest {
				srcByID := map[int64]*store.Source{}
				for _, src := range subs[subID].Sources {
					srcByID[src.ID] = src
				}
				subscriptions.SortNewestFirst(sel, srcByID)
				sel = sel[:req.Latest]
			}
			entries = sel
		} else if entries, err = tx.EntriesByID(ctx, ids, p); err != nil {
			return err
		}
		groups := map[int64][]*store.Entry{}
		var order []int64
		for _, e := range entries {
			if e.ItemActive() || e.InArchive && !req.Force || subs[e.SubscriptionID] == nil {
				res.Skipped++
				continue
			}
			if groups[e.SubscriptionID] == nil {
				order = append(order, e.SubscriptionID)
			}
			groups[e.SubscriptionID] = append(groups[e.SubscriptionID], e)
		}
		for _, sid := range order {
			j, err := m.subJobTx(ctx, tx, subs[sid], opts[sid], band, groups[sid], nil)
			if err != nil {
				return err
			}
			for _, e := range groups[sid] {
				if err := tx.UpdateEntry(ctx, e); err != nil {
					return err
				}
			}
			jobs = append(jobs, j)
			res.Queued += len(groups[sid])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		m.Logs.Note(j.ID, "Job created from subscription %q", subs[j.SubscriptionID].Name)
		m.Sched.Enqueue(scheduler.Job{ID: j.ID, Pos: j.QueuePos, Site: j.Site, Band: j.Band})
		m.publishJob(j.ID)
		if full, err := m.Store.GetJob(ctx, j.ID); err == nil {
			res.Jobs = append(res.Jobs, m.view(full))
		}
	}
	for id := range subIDs {
		m.markCounts(id)
		m.Hub.Publish(EventEntries, map[string]any{"subscription": itoa(id), "changed": res.Queued})
	}
	return res, nil
}

// IgnoreEntries sets or clears the user decision "ignored".
func (m *Manager) IgnoreEntries(ctx context.Context, idStrs []string, ignored bool) (int, error) {
	var ids []int64
	for _, s := range idStrs {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, invalid("invalid entry id %q", s)
		}
		ids = append(ids, id)
	}
	es, err := m.Store.EntriesByID(ctx, ids, m.statusParams())
	if err != nil {
		return 0, err
	}
	n, err := m.Store.SetIgnored(ctx, ids, ignored)
	if err != nil {
		return 0, err
	}
	subs := map[int64]bool{}
	for _, e := range es {
		subs[e.SubscriptionID] = true
	}
	for id := range subs {
		m.markCounts(id)
		m.Hub.Publish(EventEntries, map[string]any{"subscription": itoa(id), "changed": n})
	}
	return n, nil
}

// --- preview ---

// PreviewEntry is one of the first entries of a preview.
type PreviewEntry struct {
	Title      string  `json:"title"`
	URL        string  `json:"url"`
	Key        string  `json:"key"`
	Duration   float64 `json:"duration,omitempty"`
	UploadDate string  `json:"uploadDate,omitempty"`
	Thumbnail  string  `json:"thumbnail,omitempty"`
	LiveStatus string  `json:"liveStatus,omitempty"`
	InLibrary  bool    `json:"inLibrary"`
}

// PreviewSource is a source the add dialog offers (a channel tab or the
// playlist itself).
type PreviewSource struct {
	Tab          string `json:"tab,omitempty"`
	URL          string `json:"url"`
	Canonical    string `json:"canonical"`
	Selected     bool   `json:"selected"`
	Subscription string `json:"subscriptionId,omitempty"` // already subscribed in
	Name         string `json:"subscriptionName,omitempty"`
}

// Preview is the result of POST /subscriptions/preview.
type Preview struct {
	URL          string          `json:"url"`                    // the listed URL
	ResolvedFrom string          `json:"resolvedFrom,omitempty"` // video title, if a video URL was resolved to its channel
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Uploader     string          `json:"uploader"`
	Kind         string          `json:"kind"`
	Thumbnail    string          `json:"thumbnail"`
	Count        int             `json:"count,omitempty"` // total if the site reports it
	Entries      []PreviewEntry  `json:"entries"`
	InLibrary    int             `json:"inLibrary"`
	Sources      []PreviewSource `json:"sources"`
}

// PreviewError carries a classified listing failure.
type PreviewError struct{ Class ytdlp.Classification }

func (e *PreviewError) Error() string { return e.Class.Reason }

// PreviewSubscription lists the first entries of a URL (§6). A YouTube
// channel root lists its Videos tab and offers the tabs; a video URL is
// resolved to its channel.
func (m *Manager) PreviewSubscription(ctx context.Context, raw string) (*Preview, error) {
	raw = strings.TrimSpace(raw)
	if err := config.ValidateURL(raw); err != nil {
		return nil, invalid("%v", err)
	}
	settings := m.Settings.Settings()
	opts, err := config.Resolve(config.OptionsRequest{}, &settings, m.siteRules(), raw)
	if err != nil {
		return nil, invalid("%v", err)
	}
	bin, release, err := m.resolver.Acquire(ctx, opts.YtdlpVersion)
	if err != nil {
		return nil, err
	}
	defer release()
	call := ytdlp.CallOptions{Impersonate: opts.Impersonate, SleepRequests: opts.SleepRequests,
		CacheDir: filepath.Join(m.dataDir, "ytdlp", "cache")}
	if opts.CookieProfile != "" {
		dir, err := os.MkdirTemp(filepath.Join(m.dataDir, "tmp"), "preview-")
		if err == nil {
			defer os.RemoveAll(dir)
			if m.copyCookies(ctx, opts.CookieProfile, filepath.Join(dir, ytdlp.CookieFile)) == nil {
				call.CookiesFile = filepath.Join(dir, ytdlp.CookieFile)
			}
		}
	}
	list := func(u string) (*ytdlp.Listing, error) {
		lctx, cancel := context.WithTimeout(ctx, subscriptions.PreviewTimeout)
		defer cancel()
		_, tab, _ := subscriptions.YouTubeChannel(u)
		res := ytdlp.Run(lctx, ytdlp.Call{Bin: bin.Path, CaptureStdout: true,
			Args: ytdlp.ListArgs(u, ytdlp.ListSpec{Window: 10, YouTubeTab: tab != ""}, call)})
		if !res.OK() {
			cls := subscriptions.ListingClass(ytdlp.Classify(res))
			if lctx.Err() != nil && ctx.Err() == nil {
				cls = ytdlp.Classification{Class: ytdlp.ClassTransient, Reason: "the preview timed out"}
			}
			return nil, &PreviewError{Class: cls}
		}
		l, err := ytdlp.ParseListing(res.Stdout)
		if err != nil {
			return nil, &PreviewError{Class: ytdlp.Classification{Class: ytdlp.ClassUnknown, Reason: err.Error()}}
		}
		return l, nil
	}
	target := raw
	if base, tab, ok := subscriptions.YouTubeChannel(raw); ok && tab == "" {
		target = subscriptions.TabURL(base, subscriptions.TabVideos)
	}
	l, err := list(target)
	if err != nil {
		return nil, err
	}
	p := &Preview{Entries: []PreviewEntry{}, Sources: []PreviewSource{}}
	if !l.IsPlaylist {
		// A video: subscribe to its channel instead (§14 decision 20).
		if l.ChannelURL == "" || config.ValidateURL(l.ChannelURL) != nil {
			return nil, fmt.Errorf("%w: this video does not name its channel; subscribe to the channel or user page instead", ErrUnprocessable)
		}
		if len(l.Entries) > 0 {
			p.ResolvedFrom = l.Entries[0].Title
		}
		target = l.ChannelURL
		if base, tab, ok := subscriptions.YouTubeChannel(target); ok && tab == "" {
			target = subscriptions.TabURL(base, subscriptions.TabVideos)
		}
		if l, err = list(target); err != nil {
			return nil, err
		}
		if !l.IsPlaylist {
			return nil, fmt.Errorf("%w: the channel of this video cannot be listed", ErrUnprocessable)
		}
	}
	p.URL, p.Name, p.Title, p.Kind, p.Thumbnail, p.Count = target, subscriptions.DisplayName(l), l.Title,
		subscriptions.Kind(l), l.Thumbnail, l.Count
	p.Uploader = l.Channel
	if p.Uploader == "" {
		p.Uploader = l.Uploader
	}
	for i, le := range l.Entries {
		if i >= 10 {
			break
		}
		key := subscriptions.EntryKey(le)
		in, _ := m.Store.HasArchive(ctx, key)
		if in {
			p.InLibrary++
		}
		p.Entries = append(p.Entries, PreviewEntry{Title: le.Title, URL: le.URL, Key: key, Duration: le.Duration,
			UploadDate: le.UploadDate, Thumbnail: le.Thumbnail, LiveStatus: le.LiveStatus, InLibrary: in})
	}
	canon, tab := subscriptions.Canonical(target, l)
	if base, _, ok := subscriptions.YouTubeChannel(target); ok && tab != "" {
		id := ""
		if c, ok := strings.CutPrefix(canon, "https://www.youtube.com/channel/"); ok {
			id, _, _ = strings.Cut(c, "/")
		}
		for _, t := range subscriptions.Tabs {
			u := subscriptions.TabURL(base, t)
			c, _ := subscriptions.Canonical(u, nil)
			if id != "" {
				c = subscriptions.ChannelTabURL(id, t)
			}
			p.Sources = append(p.Sources, PreviewSource{Tab: t, URL: u, Canonical: c, Selected: t == tab})
		}
	} else {
		p.Sources = append(p.Sources, PreviewSource{URL: target, Canonical: canon, Selected: true})
	}
	var canons []string
	for _, s := range p.Sources {
		canons = append(canons, s.Canonical)
	}
	existing, err := m.Store.SourcesByCanonical(ctx, canons)
	if err != nil {
		return nil, err
	}
	names := m.subNames(ctx)
	for i := range p.Sources {
		for _, src := range existing {
			if src.Canonical == p.Sources[i].Canonical {
				p.Sources[i].Subscription, p.Sources[i].Name = itoa(src.SubscriptionID), names[src.SubscriptionID]
			}
		}
	}
	return p, nil
}

// SubscriptionsUsing lists the subscriptions whose own options use a
// library, preset or cookie profile (shown before it is removed, §5.2).
func (m *Manager) SubscriptionsUsing(ctx context.Context) (map[string]map[string][]string, error) {
	subs, err := m.Store.ListSubscriptions(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string][]string{"library": {}, "preset": {}, "cookieProfile": {}}
	add := func(kind string, v *string, name string) {
		if v != nil && *v != "" {
			out[kind][*v] = append(out[kind][*v], name)
		}
	}
	for _, s := range subs {
		add("library", s.Requested.Library, s.Name)
		add("preset", s.Requested.Preset, s.Name)
		add("cookieProfile", s.Requested.CookieProfile, s.Name)
	}
	for _, m := range out {
		for _, names := range m {
			sort.Strings(names)
		}
	}
	return out, nil
}
