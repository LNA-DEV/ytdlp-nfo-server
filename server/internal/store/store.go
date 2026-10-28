// Package store persists jobs, items, the archive and settings in SQLite
// (modernc.org/sqlite, CGO-free) with versioned schema migrations.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct {
	db     *sql.DB
	path   string
	memory bool
}

// Open opens (and migrates) the database at path. An empty path opens a
// private in-memory database.
func Open(path string) (*Store, error) {
	var dsn string
	memory := path == ""
	params := url.Values{}
	params.Add("_pragma", "busy_timeout(10000)")
	params.Add("_pragma", "foreign_keys(1)")
	params.Add("_txlock", "immediate")
	if memory {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		params.Add("mode", "memory")
		params.Add("cache", "shared")
		dsn = "file:mem" + hex.EncodeToString(b) + "?" + params.Encode()
	} else {
		params.Add("_pragma", "journal_mode(WAL)")
		// FULL: an archive record must survive a power loss once the item
		// is in the library (invariant 4).
		params.Add("_pragma", "synchronous(FULL)")
		dsn = "file:" + path + "?" + params.Encode()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if memory {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(4)
	}
	db.SetConnMaxIdleTime(0)
	s := &Store{db: db, path: path, memory: memory}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the connection.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// InMemory reports whether the store is not persisted.
func (s *Store) InMemory() bool { return s.memory }

var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	url TEXT NOT NULL,
	norm_url TEXT NOT NULL DEFAULT '',
	site TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	uploader TEXT NOT NULL DEFAULT '',
	thumbnail TEXT NOT NULL DEFAULT '',
	is_playlist INTEGER NOT NULL DEFAULT 0,
	state TEXT NOT NULL,
	requested TEXT NOT NULL DEFAULT '{}',
	options TEXT NOT NULL DEFAULT '{}',
	error TEXT NOT NULL DEFAULT '',
	error_class TEXT NOT NULL DEFAULT '',
	hint TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 0,
	retry TEXT NOT NULL DEFAULT '{}',
	retry_at INTEGER,
	queue_pos INTEGER NOT NULL DEFAULT 0,
	ytdlp_version TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	started_at INTEGER,
	finished_at INTEGER,
	updated_at INTEGER NOT NULL,
	legacy INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX jobs_state ON jobs(state);
CREATE INDEX jobs_finished ON jobs(finished_at);
CREATE INDEX jobs_norm_url ON jobs(norm_url);
CREATE TABLE items (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	idx INTEGER NOT NULL,
	url TEXT NOT NULL DEFAULT '',
	extractor TEXT NOT NULL DEFAULT '',
	video_id TEXT NOT NULL DEFAULT '',
	archive_key TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	playlist_title TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	error_class TEXT NOT NULL DEFAULT '',
	hint TEXT NOT NULL DEFAULT '',
	retry TEXT NOT NULL DEFAULT '{}',
	retry_at INTEGER,
	warnings TEXT NOT NULL DEFAULT '[]',
	library TEXT NOT NULL DEFAULT '',
	final_path TEXT NOT NULL DEFAULT '',
	files TEXT NOT NULL DEFAULT '[]',
	size INTEGER NOT NULL DEFAULT 0,
	duration REAL NOT NULL DEFAULT 0,
	ytdlp_version TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL,
	finished_at INTEGER
);
CREATE INDEX items_job ON items(job_id, idx);
CREATE INDEX items_key ON items(archive_key);
CREATE INDEX items_state ON items(state);
CREATE TABLE archive (
	key TEXT PRIMARY KEY,
	extractor TEXT NOT NULL,
	video_id TEXT NOT NULL,
	path TEXT NOT NULL DEFAULT '',
	job_id INTEGER,
	finalized_at INTEGER NOT NULL,
	source TEXT NOT NULL DEFAULT 'server'
);
CREATE TABLE ytdlp_versions (
	id TEXT PRIMARY KEY,
	channel TEXT NOT NULL,
	tag TEXT NOT NULL,
	version TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL,
	dir TEXT NOT NULL,
	asset TEXT NOT NULL DEFAULT '',
	sha256 TEXT NOT NULL DEFAULT '',
	bundled INTEGER NOT NULL DEFAULT 0,
	pinned INTEGER NOT NULL DEFAULT 0,
	smoke_ok INTEGER NOT NULL DEFAULT 0,
	smoke_output TEXT NOT NULL DEFAULT '',
	impersonate_targets TEXT NOT NULL DEFAULT '[]',
	installed_at INTEGER NOT NULL
);
CREATE TABLE site_rules (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	position INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE TABLE cookie_profiles (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	filename TEXT NOT NULL,
	size INTEGER NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE api_tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	hash TEXT NOT NULL UNIQUE,
	prefix TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	last_used_at INTEGER
);
CREATE TABLE sessions (
	hash TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
`,
	// 2: subscriptions (subscriptions concept §9)
	`
CREATE TABLE subscriptions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	folder TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT '',
	uploader TEXT NOT NULL DEFAULT '',
	thumbnail TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	backlog TEXT NOT NULL DEFAULT '{}',
	filters TEXT NOT NULL DEFAULT '{}',
	requested TEXT NOT NULL DEFAULT '{}',
	interval_minutes INTEGER NOT NULL DEFAULT 0,
	listing TEXT NOT NULL DEFAULT 'auto',
	notify INTEGER NOT NULL DEFAULT 0,
	state TEXT NOT NULL DEFAULT 'idle',
	failures INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	error_class TEXT NOT NULL DEFAULT '',
	hint TEXT NOT NULL DEFAULT '',
	warning TEXT NOT NULL DEFAULT '',
	last_check_at INTEGER,
	last_ok_at INTEGER,
	next_check_at INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX subscriptions_due ON subscriptions(enabled, next_check_at);
CREATE TABLE subscription_sources (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	subscription_id INTEGER NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
	url TEXT NOT NULL,
	canonical TEXT NOT NULL UNIQUE,
	tab TEXT NOT NULL DEFAULT '',
	added_at INTEGER NOT NULL,
	first_ok_at INTEGER,
	last_ok_at INTEGER,
	last_full_at INTEGER,
	impersonate INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	error_class TEXT NOT NULL DEFAULT ''
);
CREATE INDEX subscription_sources_sub ON subscription_sources(subscription_id);
CREATE TABLE subscription_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	subscription_id INTEGER NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
	key TEXT NOT NULL,
	url TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	duration REAL NOT NULL DEFAULT 0,
	upload_date TEXT NOT NULL DEFAULT '',
	sort_date INTEGER NOT NULL,
	thumbnail TEXT NOT NULL DEFAULT '',
	live_status TEXT NOT NULL DEFAULT '',
	availability TEXT NOT NULL DEFAULT '',
	source_id INTEGER REFERENCES subscription_sources(id) ON DELETE SET NULL,
	position INTEGER NOT NULL DEFAULT 0,
	backlog INTEGER NOT NULL DEFAULT 0,
	first_seen_at INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	gone_at INTEGER,
	ignored INTEGER NOT NULL DEFAULT 0,
	ignore_reason TEXT NOT NULL DEFAULT '',
	item_id INTEGER REFERENCES items(id) ON DELETE SET NULL,
	UNIQUE (subscription_id, key)
);
CREATE INDEX subscription_entries_key ON subscription_entries(key);
CREATE INDEX subscription_entries_item ON subscription_entries(item_id);
CREATE INDEX subscription_entries_source ON subscription_entries(source_id);
CREATE INDEX subscription_entries_sub_sort ON subscription_entries(subscription_id, sort_date);
CREATE INDEX subscription_entries_sort ON subscription_entries(sort_date);
CREATE INDEX subscription_entries_backlog_sort ON subscription_entries(backlog, sort_date);
CREATE INDEX subscription_entries_sub_backlog_sort ON subscription_entries(subscription_id, backlog, sort_date);
CREATE INDEX subscription_entries_ignored_sort ON subscription_entries(sort_date) WHERE ignored = 1;
CREATE INDEX subscription_entries_linked_sort ON subscription_entries(sort_date)
	WHERE item_id IS NOT NULL OR live_status IN ('is_upcoming', 'is_live', 'post_live');
CREATE INDEX subscription_entries_sub_linked_sort ON subscription_entries(subscription_id, sort_date)
	WHERE item_id IS NOT NULL OR live_status IN ('is_upcoming', 'is_live', 'post_live');
ALTER TABLE jobs ADD COLUMN subscription_id INTEGER REFERENCES subscriptions(id) ON DELETE SET NULL;
ALTER TABLE jobs ADD COLUMN band INTEGER NOT NULL DEFAULT 0;
CREATE INDEX jobs_subscription ON jobs(subscription_id);
`,
	// 3: the key a listing reports for an entry that was re-keyed by its
	// download (listings without video ids, subscriptions concept §4.2)
	`
ALTER TABLE subscription_entries ADD COLUMN listing_key TEXT NOT NULL DEFAULT '';
`,
	// 4: the order a source lists in (subscriptions concept §5.2)
	`
ALTER TABLE subscription_sources ADD COLUMN newest_first INTEGER NOT NULL DEFAULT 0;
ALTER TABLE subscription_sources ADD COLUMN order_known INTEGER NOT NULL DEFAULT 0;
UPDATE subscription_sources SET newest_first = 1, order_known = 1 WHERE tab != '';
`,
	// 5: a requested full rescan survives a restart (subscriptions concept §5.4)
	`
ALTER TABLE subscriptions ADD COLUMN full_requested INTEGER NOT NULL DEFAULT 0;
`,
	// 6: Reddit sources: the provider chain of a subscription, the media
	// kind and NSFW flag of an entry (Reddit concept §9)
	`
ALTER TABLE subscriptions ADD COLUMN provider TEXT NOT NULL DEFAULT '';
ALTER TABLE subscription_entries ADD COLUMN media_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE subscription_entries ADD COLUMN nsfw INTEGER NOT NULL DEFAULT 0;
`,
	// 7: the media index of Reddit items: every file of a finalized item
	// and the media keys of skipped duplicates (Reddit concept §7.5)
	`
CREATE TABLE media_index (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	archive_key TEXT NOT NULL REFERENCES archive(key) ON DELETE CASCADE,
	media_key TEXT NOT NULL,
	path TEXT NOT NULL,
	sha256 TEXT NOT NULL DEFAULT '',
	dhash INTEGER,
	alias INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX media_index_media ON media_index(media_key);
CREATE INDEX media_index_sha ON media_index(sha256);
CREATE INDEX media_index_archive ON media_index(archive_key);
`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version(version) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this server supports (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// --- helpers ---

func ms(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func fromMs(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.UnixMilli(n.Int64)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// nullID stores 0 as NULL (optional foreign keys).
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- meta ---

// GetMeta returns a meta value ("" if missing).
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta stores a meta value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// --- settings ---

// LoadSettings returns the stored settings, or ok=false if none exist yet.
func (s *Store) LoadSettings(ctx context.Context) (config.Settings, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'runtime'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return config.Settings{}, false, nil
	}
	if err != nil {
		return config.Settings{}, false, err
	}
	var st config.Settings
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return config.Settings{}, false, fmt.Errorf("decode settings: %w", err)
	}
	st.Normalize()
	return st, true, nil
}

// SaveSettings persists settings (without the computed Locked list).
func (s *Store) SaveSettings(ctx context.Context, st config.Settings) error {
	st.Locked = nil
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES ('runtime', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, mustJSON(st))
	return err
}
