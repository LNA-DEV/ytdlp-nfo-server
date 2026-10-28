package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const itemCols = `id, job_id, idx, url, extractor, video_id, archive_key, title, playlist_title, state, attempts,
	error, error_class, hint, retry, retry_at, warnings, library, final_path, files, size, duration,
	ytdlp_version, updated_at, finished_at`

func scanItem(r scanner) (*Item, error) {
	var (
		it                            Item
		state, retry, warnings, files string
		retryAt, finished             sql.NullInt64
		updated                       int64
	)
	if err := r.Scan(&it.ID, &it.JobID, &it.Index, &it.URL, &it.Extractor, &it.VideoID, &it.ArchiveKey,
		&it.Title, &it.PlaylistTitle, &state, &it.Attempts, &it.Error, &it.ErrorClass, &it.Hint, &retry, &retryAt,
		&warnings, &it.Library, &it.FinalPath, &files, &it.Size, &it.Duration, &it.YtdlpVersion, &updated, &finished); err != nil {
		return nil, err
	}
	it.State = ItemState(state)
	_ = json.Unmarshal([]byte(retry), &it.Retry)
	_ = json.Unmarshal([]byte(warnings), &it.Warnings)
	_ = json.Unmarshal([]byte(files), &it.Files)
	it.RetryAt = fromMs(retryAt)
	it.UpdatedAt = time.UnixMilli(updated)
	it.FinishedAt = fromMs(finished)
	return &it, nil
}

func warningsJSON(w []string) string {
	if len(w) == 0 {
		return "[]"
	}
	return mustJSON(w)
}

func filesJSON(f []FileMove) string {
	if len(f) == 0 {
		return "[]"
	}
	return mustJSON(f)
}

// CreateItems inserts items and sets their IDs.
func (s *Store) CreateItems(ctx context.Context, items []*Item) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := createItemsTx(ctx, tx, items); err != nil {
		return err
	}
	return tx.Commit()
}

func createItemsTx(ctx context.Context, tx *sql.Tx, items []*Item) error {
	now := time.Now()
	for _, it := range items {
		it.UpdatedAt = now
		res, err := tx.ExecContext(ctx, `INSERT INTO items (job_id, idx, url, extractor, video_id, archive_key, title,
			playlist_title, state, attempts, error, error_class, hint, retry, retry_at, warnings, library, final_path,
			files, size, duration, ytdlp_version, updated_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			it.JobID, it.Index, it.URL, it.Extractor, it.VideoID, it.ArchiveKey, it.Title, it.PlaylistTitle,
			string(it.State), it.Attempts, it.Error, it.ErrorClass, it.Hint, mustJSON(it.Retry), ms(it.RetryAt),
			warningsJSON(it.Warnings), it.Library, it.FinalPath, filesJSON(it.Files), it.Size, it.Duration,
			it.YtdlpVersion, now.UnixMilli(), ms(it.FinishedAt))
		if err != nil {
			return err
		}
		if it.ID, err = res.LastInsertId(); err != nil {
			return err
		}
	}
	return nil
}

func updateItemTx(ctx context.Context, ex interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, it *Item) error {
	it.UpdatedAt = time.Now()
	res, err := ex.ExecContext(ctx, `UPDATE items SET idx = ?, url = ?, extractor = ?, video_id = ?, archive_key = ?,
		title = ?, playlist_title = ?, state = ?, attempts = ?, error = ?, error_class = ?, hint = ?, retry = ?,
		retry_at = ?, warnings = ?, library = ?, final_path = ?, files = ?, size = ?, duration = ?, ytdlp_version = ?,
		updated_at = ?, finished_at = ? WHERE id = ?`,
		it.Index, it.URL, it.Extractor, it.VideoID, it.ArchiveKey, it.Title, it.PlaylistTitle, string(it.State),
		it.Attempts, it.Error, it.ErrorClass, it.Hint, mustJSON(it.Retry), ms(it.RetryAt), warningsJSON(it.Warnings),
		it.Library, it.FinalPath, filesJSON(it.Files), it.Size, it.Duration, it.YtdlpVersion,
		it.UpdatedAt.UnixMilli(), ms(it.FinishedAt), it.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateItem writes all mutable fields of an item.
func (s *Store) UpdateItem(ctx context.Context, it *Item) error {
	return updateItemTx(ctx, s.db, it)
}

// GetItem returns one item.
func (s *Store) GetItem(ctx context.Context, id int64) (*Item, error) {
	it, err := scanItem(s.db.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return it, err
}

// ItemsForJob returns the items of a job in order.
func (s *Store) ItemsForJob(ctx context.Context, jobID int64) ([]*Item, error) {
	return s.queryItems(ctx, `SELECT `+itemCols+` FROM items WHERE job_id = ? ORDER BY idx, id`, jobID)
}

// ItemsInStates returns items in the given states (all jobs).
func (s *Store) ItemsInStates(ctx context.Context, states ...ItemState) ([]*Item, error) {
	ph := make([]string, len(states))
	args := make([]any, len(states))
	for i, st := range states {
		ph[i] = "?"
		args[i] = string(st)
	}
	return s.queryItems(ctx, `SELECT `+itemCols+` FROM items WHERE state IN (`+strings.Join(ph, ",")+`) ORDER BY job_id, idx`, args...)
}

func (s *Store) queryItems(ctx context.Context, q string, args ...any) ([]*Item, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MaxItemIndex returns the highest item index of a job (-1 if none).
func (s *Store) MaxItemIndex(ctx context.Context, jobID int64) (int, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(idx) FROM items WHERE job_id = ?`, jobID).Scan(&n)
	if !n.Valid {
		return -1, err
	}
	return int(n.Int64), err
}

// ShiftItemIndexes makes room after index `after` for n new items.
func (s *Store) ShiftItemIndexes(ctx context.Context, jobID int64, after, n int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE items SET idx = idx + ? WHERE job_id = ? AND idx > ?`, n, jobID, after)
	return err
}

// DeleteItemsForJob removes all items of a job (re-probe).
func (s *Store) DeleteItemsForJob(ctx context.Context, jobID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM items WHERE job_id = ?`, jobID)
	return err
}

// FinalizeItem records the archive entry and marks the item done in one
// transaction (§4.5 step 4, invariant 4). Subscription entries linked to a
// re-keyed item follow its new key in the same transaction.
func (s *Store) FinalizeItem(ctx context.Context, it *Item, e ArchiveEntry, media ...MediaRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if e.Key != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO archive (key, extractor, video_id, path, job_id, finalized_at, source)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(key) DO UPDATE SET path = excluded.path, job_id = excluded.job_id,
			finalized_at = excluded.finalized_at, source = excluded.source`,
			e.Key, e.Extractor, e.VideoID, e.Path, e.JobID, e.FinalizedAt.UnixMilli(), e.Source); err != nil {
			return err
		}
	}
	if len(media) > 0 {
		if err := putMedia(ctx, tx, e.Key, media); err != nil {
			return err
		}
	}
	if err := updateItemTx(ctx, tx, it); err != nil {
		return err
	}
	if e.Key != "" {
		// Subscription bookkeeping must never block a finalize: if it
		// fails, only its own changes are rolled back.
		if _, err := tx.ExecContext(ctx, `SAVEPOINT rekey`); err != nil {
			return err
		}
		if err := rekeyEntries(ctx, tx, it.ID, e.Key); err != nil {
			if _, err := tx.ExecContext(ctx, `ROLLBACK TO rekey`); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `RELEASE rekey`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// URLsByArchiveKey returns the most recent item URL per archive key.
func (s *Store) URLsByArchiveKey(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT archive_key, url FROM items WHERE archive_key != '' AND url != '' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, u string
		if err := rows.Scan(&k, &u); err != nil {
			return nil, err
		}
		out[k] = u // later rows (newer items) win
	}
	return out, rows.Err()
}
