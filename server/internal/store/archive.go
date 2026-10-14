package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// HasArchive reports whether a key is in the archive.
func (s *Store) HasArchive(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM archive WHERE key = ?`, key).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// GetArchive returns one entry.
func (s *Store) GetArchive(ctx context.Context, key string) (*ArchiveEntry, error) {
	rows, err := s.queryArchive(ctx, `WHERE key = ?`, key)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return &rows[0], nil
}

// ImportArchiveKeys inserts keys that are not yet present and returns how
// many were new.
func (s *Store) ImportArchiveKeys(ctx context.Context, keys []string, source string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	n := 0
	for _, k := range keys {
		ext, id, ok := strings.Cut(k, " ")
		if !ok || ext == "" || id == "" {
			continue
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO archive (key, extractor, video_id, finalized_at, source)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT(key) DO NOTHING`, k, ext, id, now, source)
		if err != nil {
			return 0, err
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n++
		}
	}
	return n, tx.Commit()
}

// RemoveArchive deletes an entry.
func (s *Store) RemoveArchive(ctx context.Context, key string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM archive WHERE key = ?`, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SearchArchive lists entries matching q (key or path), newest first.
func (s *Store) SearchArchive(ctx context.Context, q string, limit, offset int) ([]ArchiveEntry, int, error) {
	where := ""
	var args []any
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		where = `WHERE key LIKE ? ESCAPE '\' OR path LIKE ? ESCAPE '\'`
		args = append(args, like, like)
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM archive `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 100
	}
	args = append(args, limit, max(offset, 0))
	rows, err := s.queryArchive(ctx, where+` ORDER BY finalized_at DESC, key LIMIT ? OFFSET ?`, args...)
	return rows, total, err
}

// AllArchiveKeys returns every key (export).
func (s *Store) AllArchiveKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM archive ORDER BY finalized_at, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) queryArchive(ctx context.Context, where string, args ...any) ([]ArchiveEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, extractor, video_id, path, job_id, finalized_at, source FROM archive `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArchiveEntry
	for rows.Next() {
		var e ArchiveEntry
		var job sql.NullInt64
		var fin int64
		if err := rows.Scan(&e.Key, &e.Extractor, &e.VideoID, &e.Path, &job, &fin, &e.Source); err != nil {
			return nil, err
		}
		e.JobID = job.Int64
		e.FinalizedAt = time.UnixMilli(fin)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AllArchive returns every archive entry.
func (s *Store) AllArchive(ctx context.Context) ([]ArchiveEntry, error) {
	return s.queryArchive(ctx, `ORDER BY finalized_at, key`)
}

// RemoveArchiveKeys deletes several entries and returns how many existed.
func (s *Store) RemoveArchiveKeys(ctx context.Context, keys []string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, k := range keys {
		res, err := tx.ExecContext(ctx, `DELETE FROM archive WHERE key = ?`, k)
		if err != nil {
			return 0, err
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n++
		}
	}
	return n, tx.Commit()
}
