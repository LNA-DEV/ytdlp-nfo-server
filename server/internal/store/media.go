package store

import (
	"context"
	"database/sql"
	"errors"
)

// MediaRow is a row of the media index (Reddit concept §7.5): a file of a
// finalized Reddit item, or the media key of a skipped duplicate (alias)
// that points at the file it duplicates.
type MediaRow struct {
	ArchiveKey string `json:"archiveKey"`
	MediaKey   string `json:"mediaKey"`
	Path       string `json:"path"` // relative to the library root
	SHA256     string `json:"sha256,omitempty"`
	Alias      bool   `json:"alias,omitempty"`
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

// putMedia records the files of an archive entry, replacing its earlier
// files (a forced re-download), and the aliases. An alias names its
// target by SHA-256 (the identical file) or by ArchiveKey and Path; a
// target that left the library meanwhile, or a media key that is already
// indexed, writes nothing.
func putMedia(ctx context.Context, q querier, archiveKey string, rows []MediaRow) error {
	if archiveKey != "" {
		if _, err := q.ExecContext(ctx, `DELETE FROM media_index WHERE archive_key = ? AND alias = 0`, archiveKey); err != nil {
			return err
		}
	}
	for _, r := range rows {
		var err error
		switch {
		case !r.Alias:
			_, err = q.ExecContext(ctx, `INSERT INTO media_index (archive_key, media_key, path, sha256) VALUES (?, ?, ?, ?)`,
				archiveKey, r.MediaKey, r.Path, r.SHA256)
		case r.SHA256 != "":
			_, err = q.ExecContext(ctx, `INSERT INTO media_index (archive_key, media_key, path, sha256, alias)
				SELECT archive_key, ?, path, sha256, 1 FROM media_index WHERE sha256 = ? AND alias = 0
				AND NOT EXISTS (SELECT 1 FROM media_index WHERE media_key = ?) ORDER BY id LIMIT 1`, r.MediaKey, r.SHA256, r.MediaKey)
		default:
			_, err = q.ExecContext(ctx, `INSERT INTO media_index (archive_key, media_key, path, sha256, alias)
				SELECT ?, ?, ?, '', 1 WHERE EXISTS (SELECT 1 FROM archive WHERE key = ?)
				AND NOT EXISTS (SELECT 1 FROM media_index WHERE media_key = ?)`, r.ArchiveKey, r.MediaKey, r.Path, r.ArchiveKey, r.MediaKey)
		}
		if err != nil {
			return err
		}
	}
	return nil
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
	if err := putMedia(ctx, tx, "", aliases); err != nil {
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
