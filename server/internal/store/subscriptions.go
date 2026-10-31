package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
	"github.com/LNA-DEV/ytdlp-nfo-server/internal/ytdlp"
)

// Subscription modes.
const (
	ModeAuto   = "auto"
	ModeManual = "manual"
)

// Listing strategies (subscriptions concept §5.2).
const (
	ListingAuto   = "auto"   // window for newest-first sources, full otherwise
	ListingWindow = "window" // always a window
	ListingFull   = "full"   // always a full listing
)

// Subscription check states.
const (
	SubIdle     = "idle"
	SubChecking = "checking"
	SubError    = "error"
)

// Backlog choices (S4).
const (
	BacklogNone   = "none"
	BacklogLatest = "latest"
	BacklogAll    = "all"
)

// Derived entry statuses (subscriptions concept §4.2), in precedence order.
const (
	EntryQueued   = "queued"
	EntryLibrary  = "library"
	EntryIgnored  = "ignored"
	EntryUpcoming = "upcoming"
	EntryFailed   = "failed"
	EntryNew      = "new"
	EntryBacklog  = "backlog"
)

// Live statuses that make an entry wait (§5.3).
var waitingLiveStatus = []string{"is_upcoming", "is_live", "post_live"}

// IsWaitingLiveStatus reports whether a listing's live_status means the
// video is not downloadable yet (upcoming, live or still processing).
func IsWaitingLiveStatus(s string) bool {
	for _, x := range waitingLiveStatus {
		if s == x {
			return true
		}
	}
	return false
}

// BacklogChoice is applied by the first check and then cleared.
type BacklogChoice struct {
	Mode string `json:"mode,omitempty"` // none (default), latest, all
	N    int    `json:"n,omitempty"`    // for latest
}

// Pending reports whether the choice still has to be applied.
func (b BacklogChoice) Pending() bool {
	return b.Mode == BacklogAll || b.Mode == BacklogLatest && b.N > 0
}

// Filters decide which entries are ignored automatically (§4.1). They use
// listing data only; an unknown value passes.
type Filters struct {
	MinDuration    float64 `json:"minDuration,omitempty"`    // seconds
	MaxDuration    float64 `json:"maxDuration,omitempty"`    // seconds
	TitleInclude   string  `json:"titleInclude,omitempty"`   // RE2, case-insensitive
	TitleExclude   string  `json:"titleExclude,omitempty"`   // RE2, case-insensitive
	PublishedAfter string  `json:"publishedAfter,omitempty"` // YYYYMMDD
	// Reddit sources (Reddit concept §7.1): media kinds to keep (image,
	// gallery, video, gif, linked; empty = all) and NSFW: include (""),
	// exclude or only.
	MediaKinds []string `json:"mediaKinds,omitempty"`
	NSFW       string   `json:"nsfw,omitempty"`
}

// Empty reports whether no filter is set.
func (f Filters) Empty() bool {
	return f.MinDuration == 0 && f.MaxDuration == 0 && f.TitleInclude == "" && f.TitleExclude == "" &&
		f.PublishedAfter == "" && len(f.MediaKinds) == 0 && (f.NSFW == "" || f.NSFW == NSFWInclude)
}

// NSFW filter values.
const (
	NSFWInclude = "include"
	NSFWExclude = "exclude"
	NSFWOnly    = "only"
)

// Subscription watches one or more listing URLs (sources).
type Subscription struct {
	ID              int64
	Name            string
	Folder          string // '' = Name
	Kind            string // channel | playlist
	Uploader        string
	Thumbnail       string
	Mode            string
	Enabled         bool
	Backlog         BacklogChoice
	Filters         Filters
	Requested       config.OptionsRequest
	IntervalMinutes int // 0 = global default
	Listing         string
	Notify          bool
	State           string
	Failures        int
	Error           string
	ErrorClass      string
	Hint            string
	Warning         string
	LastCheckAt     time.Time
	LastOKAt        time.Time
	NextCheckAt     time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// Provider overrides the Reddit provider chain ("" = the global one,
	// else providers joined by commas; Reddit concept §9).
	Provider string
	// FullRequested: the next check lists every source completely (Full
	// rescan). Only RequestFullCheck and ClearFullRequest write it.
	FullRequested bool
	// Backfill is a requested loading of older posts (Reddit concept §7.7),
	// nil if none. Only SetBackfill writes it.
	Backfill *Backfill

	Sources []*Source
}

// Kinds of backfill (Reddit concept §7.7).
const (
	BackfillTop      = "top"      // the top N of a period, from native
	BackfillRange    = "range"    // everything in a time range, from Arctic Shift
	BackfillRangeTop = "rangeTop" // the top N of a time range, sorted locally
)

// Backfill is a request to load older posts into the backlog, and its
// progress.
type Backfill struct {
	ID     int64     `json:"id"` // when it was requested (Unix ms): a check only updates the request it ran
	Kind   string    `json:"kind"`
	Period string    `json:"period,omitempty"` // top: hour … all
	N      int       `json:"n,omitempty"`      // top, rangeTop
	From   time.Time `json:"from,omitempty"`   // range, rangeTop; zero: the beginning
	To     time.Time `json:"to,omitempty"`     // zero: now
	// Download queues the loaded posts at once in the backlog band.
	Download bool `json:"download,omitempty"`
	// Pending: the next check runs it. A range that stopped at backfillMax
	// waits for Continue, which resumes before the oldest post loaded.
	Pending bool      `json:"pending"`
	Before  time.Time `json:"before,omitempty"`
	Loaded  int       `json:"loaded,omitempty"`
	Total   int       `json:"total,omitempty"` // about, from the count; 0 = unknown
	Error   string    `json:"error,omitempty"` // the last run failed
}

// EffectiveFolder returns the library folder of the subscription's items.
func (s *Subscription) EffectiveFolder() string {
	if f := strings.TrimSpace(s.Folder); f != "" {
		return f
	}
	return s.Name
}

// Source is one listing URL of a subscription.
type Source struct {
	ID             int64
	SubscriptionID int64
	URL            string // as entered
	Canonical      string // unique across all subscriptions
	Tab            string // YouTube tab: videos, shorts, streams; else ''
	AddedAt        time.Time
	FirstOKAt      time.Time // zero: the next successful listing records backlog
	LastOKAt       time.Time
	LastFullAt     time.Time
	Impersonate    bool
	// NewestFirst: the source lists its newest entry first (a channel
	// tab), else oldest first (a playlist). OrderKnown: the order was
	// confirmed (a YouTube tab, dates in the listing, or where new entries
	// appeared); until then NewestFirst is a guess and checks list the
	// source completely (§5.2).
	NewestFirst bool
	OrderKnown  bool
	Error       string
	ErrorClass  string
	// ListLimit is N of a top list (Reddit concept §7.7): every check lists
	// exactly the top N. 0 for all other sources.
	ListLimit int
}

// Entry is one video seen in a source. Status and the Item* fields are
// derived at read time (S2).
type Entry struct {
	ID             int64
	SubscriptionID int64
	Key            string // archive key, or "url <normalized url>"
	// ListingKey is the key the listings report when it differs from Key:
	// an entry without a video id in the listing ("url …") is re-keyed to
	// the archive key by its download, but listings keep reporting it by
	// URL (§4.2).
	ListingKey   string
	URL          string
	Title        string
	Duration     float64
	UploadDate   string
	SortDate     time.Time
	Thumbnail    string
	LiveStatus   string
	Availability string
	// MediaKind and NSFW come from Reddit listings ("" and false elsewhere).
	MediaKind    string
	NSFW         bool
	SourceID     int64
	Position     int
	Backlog      bool
	FirstSeenAt  time.Time
	LastSeenAt   time.Time
	GoneAt       time.Time
	Ignored      bool
	IgnoreReason string
	ItemID       int64

	// Derived (read only).
	ItemState      ItemState
	ItemError      string
	ItemErrorClass string
	ItemHint       string
	JobID          int64
	InArchive      bool
	Status         string
}

// ItemActive reports whether the linked item is still queued or running.
func (e *Entry) ItemActive() bool {
	return e.ItemID != 0 && e.ItemState != "" && !e.ItemState.Terminal()
}

// NotYetAvailable reports whether the linked item failed because the
// video was not available yet (premiere without live_status, §5.3).
func (e *Entry) NotYetAvailable() bool {
	return e.ItemState == ItemFailed && e.ItemError == ytdlp.ReasonNotYetAvailable
}

// SourceConflictError is returned when a source is already subscribed.
type SourceConflictError struct {
	Canonical      string
	SubscriptionID int64
}

func (e *SourceConflictError) Error() string {
	return fmt.Sprintf("%s is already subscribed (subscription %d)", e.Canonical, e.SubscriptionID)
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx is a write transaction (the database is opened with _txlock=immediate,
// so it holds the write lock from the start and reads are consistent).
type Tx struct{ tx *sql.Tx }

// InTx runs fn in one transaction and commits if it returns nil.
func (s *Store) InTx(ctx context.Context, fn func(tx *Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&Tx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// --- subscriptions ---

const subCols = `id, name, folder, kind, uploader, thumbnail, mode, enabled, backlog, filters, requested,
	interval_minutes, listing, notify, state, failures, error, error_class, hint, warning,
	last_check_at, last_ok_at, next_check_at, created_at, updated_at, full_requested, provider, backfill`

func scanSub(r scanner) (*Subscription, error) {
	var (
		s                           Subscription
		enabled, notify, full       int
		backlog, filters, requested string
		backfill                    string
		lastCheck, lastOK, next     sql.NullInt64
		created, updated            int64
	)
	if err := r.Scan(&s.ID, &s.Name, &s.Folder, &s.Kind, &s.Uploader, &s.Thumbnail, &s.Mode, &enabled, &backlog,
		&filters, &requested, &s.IntervalMinutes, &s.Listing, &notify, &s.State, &s.Failures, &s.Error,
		&s.ErrorClass, &s.Hint, &s.Warning, &lastCheck, &lastOK, &next, &created, &updated, &full, &s.Provider,
		&backfill); err != nil {
		return nil, err
	}
	s.Enabled, s.Notify, s.FullRequested = enabled != 0, notify != 0, full != 0
	_ = json.Unmarshal([]byte(backlog), &s.Backlog)
	_ = json.Unmarshal([]byte(filters), &s.Filters)
	_ = json.Unmarshal([]byte(requested), &s.Requested)
	if backfill != "" {
		s.Backfill = &Backfill{}
		if json.Unmarshal([]byte(backfill), s.Backfill) != nil {
			s.Backfill = nil
		}
	}
	s.LastCheckAt, s.LastOKAt, s.NextCheckAt = fromMs(lastCheck), fromMs(lastOK), fromMs(next)
	s.CreatedAt, s.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	return &s, nil
}

const sourceCols = `id, subscription_id, url, canonical, tab, added_at, first_ok_at, last_ok_at, last_full_at,
	impersonate, newest_first, order_known, error, error_class, list_limit`

func scanSource(r scanner) (*Source, error) {
	var (
		src                    Source
		added                  int64
		firstOK, lastOK, lastF sql.NullInt64
		imp, newest, known     int
	)
	if err := r.Scan(&src.ID, &src.SubscriptionID, &src.URL, &src.Canonical, &src.Tab, &added, &firstOK, &lastOK,
		&lastF, &imp, &newest, &known, &src.Error, &src.ErrorClass, &src.ListLimit); err != nil {
		return nil, err
	}
	src.NewestFirst, src.OrderKnown = newest != 0, known != 0
	src.AddedAt = time.UnixMilli(added)
	src.FirstOKAt, src.LastOKAt, src.LastFullAt = fromMs(firstOK), fromMs(lastOK), fromMs(lastF)
	src.Impersonate = imp != 0
	return &src, nil
}

func querySubs(ctx context.Context, q querier, where string, args ...any) ([]*Subscription, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+subCols+` FROM subscriptions `+where, args...)
	if err != nil {
		return nil, err
	}
	var subs []*Subscription
	byID := map[int64]*Subscription{}
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		sub.Sources = []*Source{}
		subs = append(subs, sub)
		byID[sub.ID] = sub
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(subs) == 0 {
		return subs, err
	}
	// Only the sources of these subscriptions (all of them for a long list).
	srcWhere, srcArgs := `ORDER BY id`, []any(nil)
	if len(subs) <= 500 {
		for _, sub := range subs {
			srcArgs = append(srcArgs, sub.ID)
		}
		srcWhere = `WHERE subscription_id IN (` + placeholders(len(subs)) + `) ORDER BY id`
	}
	srcs, err := querySources(ctx, q, srcWhere, srcArgs...)
	if err != nil {
		return nil, err
	}
	for _, src := range srcs {
		if sub := byID[src.SubscriptionID]; sub != nil {
			sub.Sources = append(sub.Sources, src)
		}
	}
	return subs, nil
}

func querySources(ctx context.Context, q querier, where string, args ...any) ([]*Source, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+sourceCols+` FROM subscription_sources `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

func getSub(ctx context.Context, q querier, id int64) (*Subscription, error) {
	subs, err := querySubs(ctx, q, `WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, ErrNotFound
	}
	return subs[0], nil
}

// checkCanonical returns a *SourceConflictError if one of the canonical
// URLs is already used by a source (of any subscription).
func checkCanonical(ctx context.Context, q querier, canonicals []string) error {
	seen := map[string]bool{}
	for _, c := range canonicals {
		if seen[c] {
			return &SourceConflictError{Canonical: c}
		}
		seen[c] = true
		var subID int64
		err := q.QueryRowContext(ctx, `SELECT subscription_id FROM subscription_sources WHERE canonical = ?`, c).Scan(&subID)
		if err == nil {
			return &SourceConflictError{Canonical: c, SubscriptionID: subID}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}

func insertSources(ctx context.Context, q querier, subID int64, srcs []*Source) error {
	canon := make([]string, len(srcs))
	for i, src := range srcs {
		canon[i] = src.Canonical
	}
	if err := checkCanonical(ctx, q, canon); err != nil {
		return err
	}
	now := time.Now()
	for _, src := range srcs {
		src.SubscriptionID = subID
		if src.AddedAt.IsZero() {
			src.AddedAt = now
		}
		res, err := q.ExecContext(ctx, `INSERT INTO subscription_sources (subscription_id, url, canonical, tab, added_at,
			first_ok_at, last_ok_at, last_full_at, impersonate, newest_first, order_known, error, error_class, list_limit)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			subID, src.URL, src.Canonical, src.Tab, src.AddedAt.UnixMilli(), ms(src.FirstOKAt), ms(src.LastOKAt),
			ms(src.LastFullAt), b2i(src.Impersonate), b2i(src.NewestFirst), b2i(src.OrderKnown), src.Error, src.ErrorClass,
			src.ListLimit)
		if err != nil {
			return err
		}
		if src.ID, err = res.LastInsertId(); err != nil {
			return err
		}
	}
	return nil
}

// CreateSubscription inserts a subscription with its sources. It returns a
// *SourceConflictError if a source is already subscribed.
func (s *Store) CreateSubscription(ctx context.Context, sub *Subscription) error {
	return s.InTx(ctx, func(tx *Tx) error { return tx.CreateSubscription(ctx, sub) })
}

// CreateSubscription inserts a subscription with its sources.
func (t *Tx) CreateSubscription(ctx context.Context, sub *Subscription) error {
	now := time.Now()
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = now
	}
	sub.UpdatedAt = now
	if sub.State == "" {
		sub.State = SubIdle
	}
	if sub.Listing == "" {
		sub.Listing = ListingAuto
	}
	res, err := t.tx.ExecContext(ctx, `INSERT INTO subscriptions (name, folder, kind, uploader, thumbnail, mode, enabled,
		backlog, filters, requested, interval_minutes, listing, notify, state, failures, error, error_class, hint, warning,
		last_check_at, last_ok_at, next_check_at, created_at, updated_at, provider)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.Name, sub.Folder, sub.Kind, sub.Uploader, sub.Thumbnail, sub.Mode, b2i(sub.Enabled), mustJSON(sub.Backlog),
		mustJSON(sub.Filters), mustJSON(sub.Requested), sub.IntervalMinutes, sub.Listing, b2i(sub.Notify), sub.State,
		sub.Failures, sub.Error, sub.ErrorClass, sub.Hint, sub.Warning, ms(sub.LastCheckAt), ms(sub.LastOKAt),
		ms(sub.NextCheckAt), sub.CreatedAt.UnixMilli(), sub.UpdatedAt.UnixMilli(), sub.Provider)
	if err != nil {
		return err
	}
	if sub.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	return insertSources(ctx, t.tx, sub.ID, sub.Sources)
}

// GetSubscription returns a subscription with its sources.
func (s *Store) GetSubscription(ctx context.Context, id int64) (*Subscription, error) {
	return getSub(ctx, s.db, id)
}

// GetSubscription reads a subscription inside the transaction.
func (t *Tx) GetSubscription(ctx context.Context, id int64) (*Subscription, error) {
	return getSub(ctx, t.tx, id)
}

// ListSubscriptions returns all subscriptions with their sources, by name.
func (s *Store) ListSubscriptions(ctx context.Context) ([]*Subscription, error) {
	return querySubs(ctx, s.db, `ORDER BY name COLLATE NOCASE, id`)
}

// DueSubscriptions returns enabled subscriptions that are due and not
// being checked, oldest due first.
func (s *Store) DueSubscriptions(ctx context.Context, now time.Time) ([]*Subscription, error) {
	return querySubs(ctx, s.db, `WHERE enabled = 1 AND state != ? AND next_check_at IS NOT NULL AND next_check_at <= ?
		ORDER BY next_check_at, id`, SubChecking, now.UnixMilli())
}

// NextCheckAt returns the earliest next_check_at of enabled subscriptions
// (zero if none).
func (s *Store) NextCheckAt(ctx context.Context) (time.Time, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MIN(next_check_at) FROM subscriptions WHERE enabled = 1 AND state != ?`, SubChecking).Scan(&n)
	return fromMs(n), err
}

// UpdateSubscription writes the user-editable fields (not the check state).
func (s *Store) UpdateSubscription(ctx context.Context, sub *Subscription) error {
	return updateSub(ctx, s.db, sub)
}

// UpdateSubscription writes the user-editable fields inside the transaction.
func (t *Tx) UpdateSubscription(ctx context.Context, sub *Subscription) error {
	return updateSub(ctx, t.tx, sub)
}

func updateSub(ctx context.Context, q querier, sub *Subscription) error {
	sub.UpdatedAt = time.Now()
	res, err := q.ExecContext(ctx, `UPDATE subscriptions SET name = ?, folder = ?, mode = ?, enabled = ?, backlog = ?,
		filters = ?, requested = ?, interval_minutes = ?, listing = ?, notify = ?, warning = ?, next_check_at = ?,
		updated_at = ?, provider = ? WHERE id = ?`,
		sub.Name, sub.Folder, sub.Mode, b2i(sub.Enabled), mustJSON(sub.Backlog), mustJSON(sub.Filters),
		mustJSON(sub.Requested), sub.IntervalMinutes, sub.Listing, b2i(sub.Notify), sub.Warning, ms(sub.NextCheckAt),
		sub.UpdatedAt.UnixMilli(), sub.Provider, sub.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SaveCheckState writes the check state and the listing metadata of a
// subscription (kind, uploader, thumbnail, backlog choice, warning).
func (t *Tx) SaveCheckState(ctx context.Context, sub *Subscription) error {
	sub.UpdatedAt = time.Now()
	res, err := t.tx.ExecContext(ctx, `UPDATE subscriptions SET kind = ?, uploader = ?, thumbnail = ?, backlog = ?,
		state = ?, failures = ?, error = ?, error_class = ?, hint = ?, warning = ?, last_check_at = ?, last_ok_at = ?,
		next_check_at = ?, updated_at = ? WHERE id = ?`,
		sub.Kind, sub.Uploader, sub.Thumbnail, mustJSON(sub.Backlog), sub.State, sub.Failures, sub.Error,
		sub.ErrorClass, sub.Hint, sub.Warning, ms(sub.LastCheckAt), ms(sub.LastOKAt), ms(sub.NextCheckAt),
		sub.UpdatedAt.UnixMilli(), sub.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RequestFullCheck makes the next check of a subscription a full rescan
// that is due now.
func (s *Store) RequestFullCheck(ctx context.Context, id int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE subscriptions SET full_requested = 1, next_check_at = ? WHERE id = ?`,
		now.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearFullRequest records that a full rescan was done.
// SetBackfill stores a backfill request (nil removes it); with now set,
// the next check is due at once.
func (s *Store) SetBackfill(ctx context.Context, id int64, b *Backfill, now time.Time) error {
	return s.InTx(ctx, func(tx *Tx) error {
		if err := tx.SetBackfill(ctx, id, b); err != nil {
			return err
		}
		if now.IsZero() {
			return nil
		}
		_, err := tx.tx.ExecContext(ctx, `UPDATE subscriptions SET next_check_at = ? WHERE id = ?`, now.UnixMilli(), id)
		return err
	})
}

// SetBackfill stores a backfill request (nil removes it).
func (t *Tx) SetBackfill(ctx context.Context, id int64, b *Backfill) error {
	v := ""
	if b != nil {
		v = mustJSON(b)
	}
	res, err := t.tx.ExecContext(ctx, `UPDATE subscriptions SET backfill = ? WHERE id = ?`, v, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (t *Tx) ClearFullRequest(ctx context.Context, id int64) error {
	_, err := t.tx.ExecContext(ctx, `UPDATE subscriptions SET full_requested = 0 WHERE id = ?`, id)
	return err
}

// SetListLimit changes N of a top-list source.
func (t *Tx) SetListLimit(ctx context.Context, subID, sourceID int64, n int) error {
	_, err := t.tx.ExecContext(ctx, `UPDATE subscription_sources SET list_limit = ? WHERE id = ? AND subscription_id = ?`,
		n, sourceID, subID)
	return err
}

// SaveSource writes the check state of a source.
func (t *Tx) SaveSource(ctx context.Context, src *Source) error {
	_, err := t.tx.ExecContext(ctx, `UPDATE subscription_sources SET canonical = ?, tab = ?, first_ok_at = ?,
		last_ok_at = ?, last_full_at = ?, impersonate = ?, newest_first = ?, order_known = ?, error = ?, error_class = ?,
		list_limit = ? WHERE id = ?`,
		src.Canonical, src.Tab, ms(src.FirstOKAt), ms(src.LastOKAt), ms(src.LastFullAt), b2i(src.Impersonate),
		b2i(src.NewestFirst), b2i(src.OrderKnown), src.Error, src.ErrorClass, src.ListLimit, src.ID)
	return err
}

// SetSubscriptionState changes the check state only (idle/checking/error).
func (s *Store) SetSubscriptionState(ctx context.Context, id int64, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE subscriptions SET state = ? WHERE id = ?`, state, id)
	return err
}

// ResetChecking puts subscriptions left in state checking (crash) back to
// idle and makes them due now (§5.5).
func (s *Store) ResetChecking(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE subscriptions SET state = ?, next_check_at = ? WHERE state = ?`,
		SubIdle, now.UnixMilli(), SubChecking)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetNextCheck sets next_check_at of the given enabled subscriptions (all
// enabled ones if ids is empty) and returns how many were changed.
func (s *Store) SetNextCheck(ctx context.Context, ids []int64, at time.Time) (int, error) {
	q := `UPDATE subscriptions SET next_check_at = ? WHERE enabled = 1`
	args := []any{at.UnixMilli()}
	if len(ids) > 0 {
		q += ` AND id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteSubscription removes a subscription with its sources and entries.
// Jobs keep running; their subscription_id becomes NULL.
func (s *Store) DeleteSubscription(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddSources adds sources to a subscription. It returns a
// *SourceConflictError if one is already subscribed.
func (t *Tx) AddSources(ctx context.Context, subID int64, srcs []*Source) error {
	return insertSources(ctx, t.tx, subID, srcs)
}

// RemoveSource deletes a source; its entries keep source_id = NULL (§5.2).
func (t *Tx) RemoveSource(ctx context.Context, subID, sourceID int64) error {
	_, err := t.tx.ExecContext(ctx, `DELETE FROM subscription_sources WHERE id = ? AND subscription_id = ?`, sourceID, subID)
	return err
}

// ResetImpersonation clears the sticky impersonation of all sources of a
// subscription (its options changed, §5.4).
func (t *Tx) ResetImpersonation(ctx context.Context, subID int64) error {
	_, err := t.tx.ExecContext(ctx, `UPDATE subscription_sources SET impersonate = 0 WHERE subscription_id = ?`, subID)
	return err
}

// SourcesByCanonical returns the sources using any of the canonical URLs.
func (s *Store) SourcesByCanonical(ctx context.Context, canonicals []string) ([]*Source, error) {
	return sourcesByCanonical(ctx, s.db, canonicals)
}

// SourcesByCanonical reads sources by canonical URL inside the transaction.
func (t *Tx) SourcesByCanonical(ctx context.Context, canonicals []string) ([]*Source, error) {
	return sourcesByCanonical(ctx, t.tx, canonicals)
}

func sourcesByCanonical(ctx context.Context, q querier, canonicals []string) ([]*Source, error) {
	if len(canonicals) == 0 {
		return nil, nil
	}
	args := make([]any, len(canonicals))
	for i, c := range canonicals {
		args[i] = c
	}
	return querySources(ctx, q, `WHERE canonical IN (`+placeholders(len(canonicals))+`)`, args...)
}

// SubscriptionJobIDs returns the open jobs of a subscription.
func (s *Store) SubscriptionJobIDs(ctx context.Context, subID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE subscription_id = ? AND state NOT IN
		('completed', 'partial', 'failed', 'cancelled') ORDER BY id`, subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// --- entries ---

// StatusParams are the inputs of the derived status besides the stored
// data: an entry that failed with "not available yet" counts as upcoming
// while it was discovered after UpcomingSince.
type StatusParams struct {
	UpcomingSince time.Time
}

// statusExpr is the SQL form of the derived status (§4.2); the first
// match wins. Only constants are inlined.
func statusExpr(p StatusParams) string {
	since := strconv.FormatInt(p.UpcomingSince.UnixMilli(), 10)
	reason := strings.ReplaceAll(ytdlp.ReasonNotYetAvailable, "'", "''")
	return `CASE
		WHEN i.id IS NOT NULL AND i.state NOT IN ('done', 'skipped', 'failed', 'cancelled') THEN 'queued'
		WHEN a.key IS NOT NULL THEN 'library'
		WHEN e.ignored = 1 THEN 'ignored'
		WHEN e.live_status IN ('is_upcoming', 'is_live', 'post_live') THEN 'upcoming'
		WHEN i.state = 'failed' AND i.error = '` + reason + `' AND e.first_seen_at > ` + since + ` THEN 'upcoming'
		WHEN i.state IN ('failed', 'cancelled') THEN 'failed'
		WHEN e.backlog = 0 THEN 'new'
		ELSE 'backlog' END`
}

// entryFrom joins an entry with its item and its archive entry: its own,
// or the one whose file the media index lists under the entry's key (a
// crosspost, an image of a gallery, a skipped duplicate; Reddit concept
// §7.5).
const entryFrom = ` FROM subscription_entries e LEFT JOIN items i ON i.id = e.item_id LEFT JOIN archive a ON a.key =
	COALESCE((SELECT m.archive_key FROM media_index m WHERE m.media_key = e.key LIMIT 1), e.key) `

func entrySelect(p StatusParams) string {
	return `SELECT e.id, e.subscription_id, e.key, e.listing_key, e.url, e.title, e.duration, e.upload_date, e.sort_date, e.thumbnail,
		e.live_status, e.availability, e.media_kind, e.nsfw, COALESCE(e.source_id, 0), e.position, e.backlog, e.first_seen_at,
		e.last_seen_at, e.gone_at, e.ignored, e.ignore_reason, COALESCE(e.item_id, 0), COALESCE(i.state, ''), COALESCE(i.error, ''),
		COALESCE(i.error_class, ''), COALESCE(i.hint, ''), COALESCE(i.job_id, 0), a.key IS NOT NULL, ` +
		statusExpr(p) + ` AS status` + entryFrom
}

func scanEntry(r scanner) (*Entry, error) {
	var (
		e                       Entry
		sortDate, first, last   int64
		gone                    sql.NullInt64
		backlog, ignored, inArc int
		nsfw                    int
		itemState               string
	)
	if err := r.Scan(&e.ID, &e.SubscriptionID, &e.Key, &e.ListingKey, &e.URL, &e.Title, &e.Duration, &e.UploadDate, &sortDate,
		&e.Thumbnail, &e.LiveStatus, &e.Availability, &e.MediaKind, &nsfw, &e.SourceID, &e.Position, &backlog, &first, &last, &gone,
		&ignored, &e.IgnoreReason, &e.ItemID, &itemState, &e.ItemError, &e.ItemErrorClass, &e.ItemHint, &e.JobID,
		&inArc, &e.Status); err != nil {
		return nil, err
	}
	e.SortDate, e.FirstSeenAt, e.LastSeenAt, e.GoneAt = time.UnixMilli(sortDate), time.UnixMilli(first), time.UnixMilli(last), fromMs(gone)
	e.Backlog, e.Ignored, e.InArchive, e.NSFW = backlog != 0, ignored != 0, inArc != 0, nsfw != 0
	e.ItemState = ItemState(itemState)
	return &e, nil
}

func queryEntries(ctx context.Context, q querier, p StatusParams, where string, args ...any) ([]*Entry, error) {
	rows, err := q.QueryContext(ctx, entrySelect(p)+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EntryKeys returns the keys and listing keys of all entries of a
// subscription (including ignored and gone ones): a windowed listing that
// contains none of them has a gap (§5.2).
func (s *Store) EntryKeys(ctx context.Context, subID int64) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM subscription_entries WHERE subscription_id = ?
		UNION SELECT listing_key FROM subscription_entries WHERE subscription_id = ? AND listing_key != ''`, subID, subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// EntriesForSubscription returns all entries of a subscription with their
// derived status (the diff of a check).
func (t *Tx) EntriesForSubscription(ctx context.Context, subID int64, p StatusParams) ([]*Entry, error) {
	return queryEntries(ctx, t.tx, p, `WHERE e.subscription_id = ? ORDER BY e.id`, subID)
}

// EntriesByID returns entries with their derived status.
func (t *Tx) EntriesByID(ctx context.Context, ids []int64, p StatusParams) ([]*Entry, error) {
	return entriesByID(ctx, t.tx, ids, p)
}

// EntriesByID returns entries with their derived status.
func (s *Store) EntriesByID(ctx context.Context, ids []int64, p StatusParams) ([]*Entry, error) {
	return entriesByID(ctx, s.db, ids, p)
}

func entriesByID(ctx context.Context, q querier, ids []int64, p StatusParams) ([]*Entry, error) {
	var out []*Entry
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		es, err := queryEntries(ctx, q, p, `WHERE e.id IN (`+placeholders(len(chunk))+`) ORDER BY e.id`, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, es...)
	}
	return out, nil
}

// InsertEntry adds an entry and sets its ID.
func (t *Tx) InsertEntry(ctx context.Context, e *Entry) error {
	res, err := t.tx.ExecContext(ctx, `INSERT INTO subscription_entries (subscription_id, key, listing_key, url, title,
		duration, upload_date, sort_date, thumbnail, live_status, availability, media_kind, nsfw, source_id, position, backlog,
		first_seen_at, last_seen_at, gone_at, ignored, ignore_reason, item_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.SubscriptionID, e.Key, e.ListingKey, e.URL, e.Title, e.Duration, e.UploadDate, e.SortDate.UnixMilli(), e.Thumbnail,
		e.LiveStatus, e.Availability, e.MediaKind, b2i(e.NSFW), nullID(e.SourceID), e.Position, b2i(e.Backlog),
		e.FirstSeenAt.UnixMilli(), e.LastSeenAt.UnixMilli(), ms(e.GoneAt), b2i(e.Ignored), e.IgnoreReason, nullID(e.ItemID))
	if err != nil {
		return err
	}
	e.ID, err = res.LastInsertId()
	return err
}

// UpdateEntry writes all stored fields of an entry.
func (t *Tx) UpdateEntry(ctx context.Context, e *Entry) error {
	_, err := t.tx.ExecContext(ctx, `UPDATE subscription_entries SET url = ?, title = ?, duration = ?, upload_date = ?,
		sort_date = ?, thumbnail = ?, live_status = ?, availability = ?, media_kind = ?, nsfw = ?, source_id = ?, position = ?,
		backlog = ?, last_seen_at = ?, gone_at = ?, ignored = ?, ignore_reason = ?, item_id = ? WHERE id = ?`,
		e.URL, e.Title, e.Duration, e.UploadDate, e.SortDate.UnixMilli(), e.Thumbnail, e.LiveStatus, e.Availability,
		e.MediaKind, b2i(e.NSFW), nullID(e.SourceID), e.Position, b2i(e.Backlog), e.LastSeenAt.UnixMilli(), ms(e.GoneAt),
		b2i(e.Ignored), e.IgnoreReason, nullID(e.ItemID), e.ID)
	return err
}

// ArchivedKeys returns which of the keys are in the archive or the media
// index.
func (t *Tx) ArchivedKeys(ctx context.Context, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	for start := 0; start < len(keys); start += 500 {
		chunk := keys[start:min(start+500, len(keys))]
		args := make([]any, len(chunk))
		for i, k := range chunk {
			args[i] = k
		}
		in := placeholders(len(chunk))
		rows, err := t.tx.QueryContext(ctx, `SELECT key FROM archive WHERE key IN (`+in+`)
			UNION SELECT media_key FROM media_index WHERE media_key IN (`+in+`)`, append(args, args...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CreateJob inserts a job inside the transaction.
func (t *Tx) CreateJob(ctx context.Context, j *Job) error { return createJobTx(ctx, t.tx, j) }

// CreateItems inserts items inside the transaction.
func (t *Tx) CreateItems(ctx context.Context, items []*Item) error {
	return createItemsTx(ctx, t.tx, items)
}

// SetIgnored sets or clears the user decision "ignored" on entries.
// Unignoring also clears an automatic reason (filter, needs cookies).
func (s *Store) SetIgnored(ctx context.Context, ids []int64, ignored bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := []any{b2i(ignored)}
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE subscription_entries SET ignored = ?, ignore_reason = ''
		WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// PruneGoneEntries deletes entries that are gone since before t and were
// never downloaded (no linked item, not in the archive).
func (s *Store) PruneGoneEntries(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM subscription_entries WHERE gone_at IS NOT NULL AND gone_at < ?
		AND item_id IS NULL AND NOT EXISTS (SELECT 1 FROM archive a WHERE a.key = subscription_entries.key)
		AND NOT EXISTS (SELECT 1 FROM media_index m WHERE m.media_key = subscription_entries.key)`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// EntryFilter selects entries for a list page.
type EntryFilter struct {
	SubscriptionID int64  // 0 = all subscriptions (feed)
	Status         string // see statusWhere; "" = all
	Query          string
	Limit          int
	Offset         int
}

// ValidEntryStatus reports whether s is a known list filter.
func ValidEntryStatus(s string) bool {
	_, _, ok := statusWhere(s)
	return ok
}

// linkedTerm is a necessary condition of the statuses that need a linked
// item or a waiting live_status. It matches the WHERE clause of the
// partial *_linked_sort indexes exactly, so rare statuses don't walk all
// entries.
const linkedTerm = `(e.item_id IS NOT NULL OR e.live_status IN ('is_upcoming', 'is_live', 'post_live'))`

// statusWhere maps a list filter to a condition on the derived status
// ($st), plus a necessary condition on stored columns that the indexes can
// use (the status is evaluated only for rows that pass it). "new" and
// "backlog" are the detail tabs: queued and upcoming entries appear in
// them, wherever they came from (§7.3). "inbox" is exactly new.
func statusWhere(s string) (cond, pre string, ok bool) {
	switch s {
	case "", "all":
		return "", "", true
	case EntryNew:
		return `($st = 'new' OR $st IN ('queued', 'upcoming'))`, `e.backlog = 0`, true
	case EntryBacklog:
		return `($st = 'backlog' OR $st IN ('queued', 'upcoming'))`, `e.backlog = 1`, true
	case "inbox":
		return `$st = 'new'`, `e.backlog = 0 AND e.ignored = 0`, true
	case EntryIgnored:
		return `$st = 'ignored'`, `e.ignored = 1`, true
	case EntryQueued, EntryUpcoming, EntryFailed:
		return `$st = '` + s + `'`, linkedTerm, true
	case EntryLibrary:
		return `$st = 'library'`, "", true
	}
	return "", "", false
}

// ListEntries returns one page of entries, newest first by sort_date. The
// query walks the sort_date index and evaluates the derived status row by
// row, so a page stops after Limit matches.
func (s *Store) ListEntries(ctx context.Context, f EntryFilter, p StatusParams) ([]*Entry, error) {
	cond, pre, ok := statusWhere(f.Status)
	if !ok {
		return nil, fmt.Errorf("unknown status %q", f.Status)
	}
	var conds []string
	var args []any
	if f.SubscriptionID != 0 {
		conds = append(conds, `e.subscription_id = ?`)
		args = append(args, f.SubscriptionID)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		conds = append(conds, `(e.title LIKE ? ESCAPE '\' OR e.url LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}
	if pre != "" {
		conds = append(conds, pre)
	}
	if cond != "" {
		conds = append(conds, strings.ReplaceAll(cond, "$st", "("+statusExpr(p)+")"))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit, max(f.Offset, 0))
	return queryEntries(ctx, s.db, p, where+` ORDER BY e.sort_date DESC, e.id DESC LIMIT ? OFFSET ?`, args...)
}

// EntryCounts summarizes the entries of a subscription by derived status.
type EntryCounts struct {
	Total           int       `json:"total"`
	New             int       `json:"new"` // the inbox
	Backlog         int       `json:"backlog"`
	Library         int       `json:"library"`
	Ignored         int       `json:"ignored"`
	Failed          int       `json:"failed"`
	Queued          int       `json:"queued"`
	Upcoming        int       `json:"upcoming"`
	Gone            int       `json:"gone"`
	NewTab          int       `json:"newTab"`     // new + queued/upcoming from new
	BacklogTab      int       `json:"backlogTab"` // backlog + queued/upcoming from the backlog
	BacklogDuration float64   `json:"backlogDuration"`
	Newest          time.Time `json:"newest,omitzero"` // newest sort_date
}

// CountEntries returns the counts per subscription (one GROUP BY over the
// same status expression; callers cache the result).
func (s *Store) CountEntries(ctx context.Context, subID int64, p StatusParams) (map[int64]EntryCounts, error) {
	where, args := "", []any{}
	if subID != 0 {
		where, args = `WHERE e.subscription_id = ?`, append(args, subID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sub, status, backlog, COUNT(*), SUM(gone), SUM(duration), MAX(sort_date) FROM
		(SELECT e.subscription_id AS sub, `+statusExpr(p)+` AS status, e.backlog AS backlog,
		e.gone_at IS NOT NULL AS gone, e.duration AS duration, e.sort_date AS sort_date`+entryFrom+where+`)
		GROUP BY sub, status, backlog`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]EntryCounts{}
	for rows.Next() {
		var (
			sub         int64
			status      string
			backlog, n  int
			gone        int
			durationSum float64
			newest      int64
		)
		if err := rows.Scan(&sub, &status, &backlog, &n, &gone, &durationSum, &newest); err != nil {
			return nil, err
		}
		c := out[sub]
		if t := time.UnixMilli(newest); t.After(c.Newest) {
			c.Newest = t
		}
		c.Total += n
		c.Gone += gone
		switch status {
		case EntryNew:
			c.New += n
			c.NewTab += n
		case EntryBacklog:
			c.Backlog += n
			c.BacklogTab += n
			c.BacklogDuration += durationSum
		case EntryLibrary:
			c.Library += n
		case EntryIgnored:
			c.Ignored += n
		case EntryFailed:
			c.Failed += n
		case EntryQueued, EntryUpcoming:
			if status == EntryQueued {
				c.Queued += n
			} else {
				c.Upcoming += n
			}
			if backlog != 0 {
				c.BacklogTab += n
			} else {
				c.NewTab += n
			}
		}
		out[sub] = c
	}
	return out, rows.Err()
}

// SubscriptionForItem returns the subscription of the job an item belongs
// to (0 if none).
func (s *Store) SubscriptionForItem(ctx context.Context, itemID int64) (int64, error) {
	var sub sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT j.subscription_id FROM items i JOIN jobs j ON j.id = i.job_id WHERE i.id = ?`, itemID).Scan(&sub)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return sub.Int64, err
}

// RekeyEntries moves the entries of an item to its archive key; the
// pipeline calls it when an item turns out to be in the library already.
func (s *Store) RekeyEntries(ctx context.Context, itemID int64, key string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := rekeyEntries(ctx, tx, itemID, key); err != nil {
		return err
	}
	return tx.Commit()
}

// rekeyEntries moves the entries of a re-keyed item to its new archive key
// (§4.2): every entry with an old key of the item gets the new key, and an
// entry that would collide with an existing entry of the same subscription
// is merged into it (the existing one is kept and takes over item_id). The
// old key stays the listing key, so the next listing still finds the entry.
func rekeyEntries(ctx context.Context, q querier, itemID int64, newKey string) error {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT key FROM subscription_entries WHERE item_id = ? AND key != ?`, itemID, newKey)
	if err != nil {
		return err
	}
	var oldKeys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		oldKeys = append(oldKeys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, old := range oldKeys {
		// Merge where the subscription already has the new key.
		if _, err := q.ExecContext(ctx, `UPDATE subscription_entries AS n SET item_id = COALESCE(o.item_id, n.item_id),
			listing_key = CASE WHEN n.listing_key = '' THEN COALESCE(NULLIF(o.listing_key, ''), o.key) ELSE n.listing_key END
			FROM subscription_entries AS o
			WHERE o.key = ? AND n.key = ? AND n.subscription_id = o.subscription_id`, old, newKey); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM subscription_entries WHERE key = ? AND subscription_id IN
			(SELECT subscription_id FROM subscription_entries WHERE key = ?)`, old, newKey); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `UPDATE subscription_entries SET key = ?,
			listing_key = CASE WHEN listing_key = '' THEN key ELSE listing_key END WHERE key = ?`, newKey, old); err != nil {
			return err
		}
	}
	return nil
}
