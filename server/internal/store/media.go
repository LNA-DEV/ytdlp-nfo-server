package store

import (
	"context"
	"database/sql"
	"errors"
	"math/bits"
	"sync"
)

// MediaRow is a row of the media index (Reddit concept §7.5): a file of a
// finalized Reddit item, or the media key of a skipped duplicate (alias)
// that points at the file it duplicates.
type MediaRow struct {
	ArchiveKey string  `json:"archiveKey"`
	MediaKey   string  `json:"mediaKey"`
	Path       string  `json:"path"` // relative to the library root
	SHA256     string  `json:"sha256,omitempty"`
	DHash      *uint64 `json:"dhash,omitempty"` // images; nil if not hashed
	Alias      bool    `json:"alias,omitempty"`
}

// InLibrary reports whether a key is in the archive or in the media index.
func (s *Store) InLibrary(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM archive WHERE key = ?
		UNION ALL SELECT 1 FROM media_index WHERE media_key = ? LIMIT 1`, key, key).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

const mediaCols = `archive_key, media_key, path, sha256, alias`

func scanMedia(r scanner) (*MediaRow, error) {
	var m MediaRow
	var alias int
	if err := r.Scan(&m.ArchiveKey, &m.MediaKey, &m.Path, &m.SHA256, &alias); err != nil {
		return nil, err
	}
	m.Alias = alias != 0
	return &m, nil
}

func (s *Store) oneMedia(ctx context.Context, where string, args ...any) (*MediaRow, error) {
	m, err := scanMedia(s.db.QueryRowContext(ctx, `SELECT `+mediaCols+` FROM media_index `+where+` LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// MediaByKey returns where a media key is in the library (a file before
// an alias), or nil.
func (s *Store) MediaByKey(ctx context.Context, key string) (*MediaRow, error) {
	return s.oneMedia(ctx, `WHERE media_key = ? ORDER BY alias, id`, key)
}

// MediaBySHA returns a library file with this SHA-256, or nil.
func (s *Store) MediaBySHA(ctx context.Context, sha string) (*MediaRow, error) {
	if sha == "" {
		return nil, nil
	}
	return s.oneMedia(ctx, `WHERE sha256 = ? AND alias = 0 ORDER BY id`, sha)
}

// MediaOf returns the rows of an archive entry.
func (s *Store) MediaOf(ctx context.Context, archiveKey string) ([]MediaRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mediaCols+` FROM media_index WHERE archive_key = ? ORDER BY id`, archiveKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaRow
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// mediaChange is what putMedia did to the hashed files.
type mediaChange struct {
	deleted bool
	added   []hashEntry
}

// putMedia records the files of an archive entry, replacing its earlier
// files (a forced re-download), and the aliases. An alias names its
// target by SHA-256 (the identical file), by ArchiveKey and Path, or by
// Path alone (a similar file); a target that left the library meanwhile,
// or a media key that is already indexed, writes nothing.
func putMedia(ctx context.Context, q querier, archiveKey string, rows []MediaRow) (mediaChange, error) {
	var ch mediaChange
	if archiveKey != "" {
		res, err := q.ExecContext(ctx, `DELETE FROM media_index WHERE archive_key = ? AND alias = 0`, archiveKey)
		if err != nil {
			return ch, err
		}
		n, _ := res.RowsAffected()
		ch.deleted = n > 0
	}
	for _, r := range rows {
		var err error
		switch {
		case !r.Alias:
			var dh any
			if r.DHash != nil {
				dh = int64(*r.DHash)
			}
			var res sql.Result
			res, err = q.ExecContext(ctx, `INSERT INTO media_index (archive_key, media_key, path, sha256, dhash) VALUES (?, ?, ?, ?, ?)`,
				archiveKey, r.MediaKey, r.Path, r.SHA256, dh)
			if err == nil && r.DHash != nil {
				id, _ := res.LastInsertId()
				ch.added = append(ch.added, hashEntry{id, *r.DHash})
			}
		case r.SHA256 != "":
			_, err = q.ExecContext(ctx, `INSERT INTO media_index (archive_key, media_key, path, sha256, alias)
				SELECT archive_key, ?, path, sha256, 1 FROM media_index WHERE sha256 = ? AND alias = 0
				AND NOT EXISTS (SELECT 1 FROM media_index WHERE media_key = ?) ORDER BY id LIMIT 1`, r.MediaKey, r.SHA256, r.MediaKey)
		case r.ArchiveKey != "":
			_, err = q.ExecContext(ctx, `INSERT INTO media_index (archive_key, media_key, path, sha256, alias)
				SELECT ?, ?, ?, '', 1 WHERE EXISTS (SELECT 1 FROM archive WHERE key = ?)
				AND NOT EXISTS (SELECT 1 FROM media_index WHERE media_key = ?)`, r.ArchiveKey, r.MediaKey, r.Path, r.ArchiveKey, r.MediaKey)
		default:
			_, err = q.ExecContext(ctx, `INSERT INTO media_index (archive_key, media_key, path, sha256, alias)
				SELECT archive_key, ?, path, '', 1 FROM media_index WHERE path = ? AND alias = 0
				AND NOT EXISTS (SELECT 1 FROM media_index WHERE media_key = ?) ORDER BY id LIMIT 1`, r.MediaKey, r.Path, r.MediaKey)
		}
		if err != nil {
			return ch, err
		}
	}
	return ch, nil
}

// hashList is the in-memory list of the dHashes of library files (Reddit
// concept §7.5 level 3): compared linearly, a million images take 16 MB
// and a few milliseconds per lookup. It is loaded on first use, extended
// by finalize and reloaded after files left the index.
type hashList struct {
	mu      sync.Mutex
	loaded  bool
	entries []hashEntry
}

type hashEntry struct {
	id   int64
	hash uint64
}

func (l *hashList) reset() {
	l.mu.Lock()
	l.loaded, l.entries = false, nil
	l.mu.Unlock()
}

func (l *hashList) apply(ch mediaChange) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case ch.deleted:
		l.loaded, l.entries = false, nil
	case l.loaded:
		l.entries = append(l.entries, ch.added...)
	}
}

// SimilarMedia returns the library file whose dHash is closest to hash,
// if it differs in at most maxDist bits, and the distance.
func (s *Store) SimilarMedia(ctx context.Context, hash uint64, maxDist int) (*MediaRow, int, error) {
	l := &s.hashes
	l.mu.Lock()
	if !l.loaded {
		rows, err := s.db.QueryContext(ctx, `SELECT id, dhash FROM media_index WHERE dhash IS NOT NULL AND alias = 0`)
		if err != nil {
			l.mu.Unlock()
			return nil, 0, err
		}
		var entries []hashEntry
		for rows.Next() {
			var e hashEntry
			var h int64
			if err := rows.Scan(&e.id, &h); err != nil {
				rows.Close()
				l.mu.Unlock()
				return nil, 0, err
			}
			e.hash = uint64(h)
			entries = append(entries, e)
		}
		rows.Close()
		l.entries, l.loaded = entries, true
	}
	best, bestDist := int64(0), maxDist+1
	for _, e := range l.entries {
		if d := bits.OnesCount64(e.hash ^ hash); d < bestDist {
			best, bestDist = e.id, d
		}
	}
	l.mu.Unlock()
	if best == 0 {
		return nil, 0, nil
	}
	m, err := s.oneMedia(ctx, `WHERE id = ?`, best)
	if m == nil && err == nil {
		s.hashes.reset() // the file left the index meanwhile
	}
	return m, bestDist, err
}

// SkipDuplicate records a skipped duplicate (Reddit concept §7.5): the
// item is saved as skipped, the aliases are written and the item's
// subscription entries follow it to its key, in one transaction.
func (s *Store) SkipDuplicate(ctx context.Context, it *Item, aliases []MediaRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := putMedia(ctx, tx, "", aliases); err != nil {
		return err
	}
	if err := updateItemTx(ctx, tx, it); err != nil {
		return err
	}
	if it.ArchiveKey != "" {
		if err := rekeyEntries(ctx, tx, it.ID, it.ArchiveKey); err != nil {
			return err
		}
	}
	return tx.Commit()
}
