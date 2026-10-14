package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const jobCols = `j.id, j.url, j.norm_url, j.site, j.title, j.uploader, j.thumbnail, j.is_playlist,
	j.state, j.requested, j.options, j.error, j.error_class, j.hint, j.attempts, j.retry, j.retry_at,
	j.queue_pos, j.ytdlp_version, j.created_at, j.started_at, j.finished_at, j.updated_at, j.legacy`

type scanner interface{ Scan(dest ...any) error }

func scanJob(r scanner, extra ...any) (*Job, error) {
	var (
		j                                Job
		isPl, legacy                     int
		state, requested, options, retry string
		retryAt, started, finished       sql.NullInt64
		created, updated                 int64
	)
	dest := []any{&j.ID, &j.URL, &j.NormURL, &j.Site, &j.Title, &j.Uploader, &j.Thumbnail, &isPl,
		&state, &requested, &options, &j.Error, &j.ErrorClass, &j.Hint, &j.Attempts, &retry, &retryAt,
		&j.QueuePos, &j.YtdlpVersion, &created, &started, &finished, &updated, &legacy}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	j.IsPlaylist = isPl != 0
	j.Legacy = legacy != 0
	j.State = JobState(state)
	_ = json.Unmarshal([]byte(requested), &j.Requested)
	_ = json.Unmarshal([]byte(options), &j.Options)
	_ = json.Unmarshal([]byte(retry), &j.Retry)
	j.RetryAt = fromMs(retryAt)
	j.CreatedAt = time.UnixMilli(created)
	j.StartedAt = fromMs(started)
	j.FinishedAt = fromMs(finished)
	j.UpdatedAt = time.UnixMilli(updated)
	return &j, nil
}

// CreateJob inserts a job at the end of the queue and sets its ID.
func (s *Store) CreateJob(ctx context.Context, j *Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := createJobTx(ctx, tx, j); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateJobs inserts several jobs in one transaction.
func (s *Store) CreateJobs(ctx context.Context, jobs []*Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, j := range jobs {
		if err := createJobTx(ctx, tx, j); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func createJobTx(ctx context.Context, tx *sql.Tx, j *Job) error {
	now := time.Now()
	if j.CreatedAt.IsZero() {
		j.CreatedAt = now
	}
	j.UpdatedAt = now
	if j.QueuePos == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(queue_pos), 0) + 1 FROM jobs`).Scan(&j.QueuePos); err != nil {
			return err
		}
	}
	var id any // nil lets SQLite assign the next ID
	if j.ID != 0 {
		id = j.ID
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO jobs (id, url, norm_url, site, title, uploader, thumbnail, is_playlist,
		state, requested, options, error, error_class, hint, attempts, retry, retry_at, queue_pos, ytdlp_version,
		created_at, started_at, finished_at, updated_at, legacy)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, j.URL, j.NormURL, j.Site, j.Title, j.Uploader, j.Thumbnail, b2i(j.IsPlaylist),
		string(j.State), mustJSON(j.Requested), mustJSON(j.Options), j.Error, j.ErrorClass, j.Hint, j.Attempts,
		mustJSON(j.Retry), ms(j.RetryAt), j.QueuePos, j.YtdlpVersion,
		j.CreatedAt.UnixMilli(), ms(j.StartedAt), ms(j.FinishedAt), j.UpdatedAt.UnixMilli(), b2i(j.Legacy))
	if err != nil {
		return err
	}
	j.ID, err = res.LastInsertId()
	return err
}

// UpdateJob writes all mutable fields of a job.
func (s *Store) UpdateJob(ctx context.Context, j *Job) error {
	j.UpdatedAt = time.Now()
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET url = ?, norm_url = ?, site = ?, title = ?, uploader = ?, thumbnail = ?,
		is_playlist = ?, state = ?, requested = ?, options = ?, error = ?, error_class = ?, hint = ?, attempts = ?,
		retry = ?, retry_at = ?, queue_pos = ?, ytdlp_version = ?, started_at = ?, finished_at = ?, updated_at = ?
		WHERE id = ?`,
		j.URL, j.NormURL, j.Site, j.Title, j.Uploader, j.Thumbnail, b2i(j.IsPlaylist), string(j.State),
		mustJSON(j.Requested), mustJSON(j.Options), j.Error, j.ErrorClass, j.Hint, j.Attempts,
		mustJSON(j.Retry), ms(j.RetryAt), j.QueuePos, j.YtdlpVersion, ms(j.StartedAt), ms(j.FinishedAt),
		j.UpdatedAt.UnixMilli(), j.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetJob returns a job with item counts.
func (s *Store) GetJob(ctx context.Context, id int64) (*Job, error) {
	jobs, err := s.queryJobs(ctx, `WHERE j.id = ?`, "", []any{id})
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, ErrNotFound
	}
	return jobs[0], nil
}

const countsJoin = `LEFT JOIN (SELECT job_id,
		COUNT(*) AS total,
		SUM(state = 'done') AS done,
		SUM(state = 'skipped') AS skipped,
		SUM(state = 'failed') AS failed,
		SUM(state NOT IN ('done', 'skipped', 'failed', 'cancelled')) AS pending,
		SUM(warnings != '[]') AS warned
	FROM items GROUP BY job_id) c ON c.job_id = j.id`

func (s *Store) queryJobs(ctx context.Context, where, order string, args []any) ([]*Job, error) {
	q := `SELECT ` + jobCols + `, COALESCE(c.total, 0), COALESCE(c.done, 0), COALESCE(c.skipped, 0),
		COALESCE(c.failed, 0), COALESCE(c.pending, 0), COALESCE(c.warned, 0)
		FROM jobs j ` + countsJoin + ` ` + where + ` ` + order
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		var c Counts
		j, err := scanJob(rows, &c.Total, &c.Done, &c.Skipped, &c.Failed, &c.Pending, &c.Warnings)
		if err != nil {
			return nil, err
		}
		j.Counts = c
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobFilter selects jobs for listing.
type JobFilter struct {
	States []JobState
	Query  string
	Limit  int
	Offset int
}

// ListJobs returns jobs matching the filter, ordered for their tab:
// queued/active by queue position, finished ones newest first.
func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]*Job, error) {
	var conds []string
	var args []any
	if len(f.States) > 0 {
		ph := make([]string, len(f.States))
		for i, st := range f.States {
			ph[i] = "?"
			args = append(args, string(st))
		}
		conds = append(conds, "j.state IN ("+strings.Join(ph, ",")+")")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		conds = append(conds, `(j.url LIKE ? ESCAPE '\' OR j.title LIKE ? ESCAPE '\' OR j.uploader LIKE ? ESCAPE '\' OR CAST(j.id AS TEXT) = ?)`)
		args = append(args, like, like, like, q)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	order := `ORDER BY CASE WHEN j.state IN ('queued', 'probing', 'running', 'retrying') THEN 0 ELSE 1 END,
		CASE WHEN j.state IN ('queued', 'probing', 'running', 'retrying') THEN j.queue_pos END ASC,
		COALESCE(j.finished_at, j.updated_at) DESC, j.id DESC`
	if f.Limit > 0 {
		order += fmt.Sprintf(" LIMIT %d OFFSET %d", f.Limit, max(f.Offset, 0))
	}
	return s.queryJobs(ctx, where, order, args)
}

// CountJobsByTab returns the number of jobs per UI tab.
func (s *Store) CountJobsByTab(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{"active": 0, "queued": 0, "completed": 0, "failed": 0}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[JobState(st).Tab()] += n
	}
	return out, rows.Err()
}

// JobsInStates returns jobs in the given states ordered by queue position.
func (s *Store) JobsInStates(ctx context.Context, states ...JobState) ([]*Job, error) {
	jobs, err := s.ListJobs(ctx, JobFilter{States: states})
	return jobs, err
}

// FindOpenJobByNormURL returns a job with the same normalized URL that is
// not completed or cancelled (v1 duplicate semantics: 409 on re-submit).
func (s *Store) FindOpenJobByNormURL(ctx context.Context, norm string) (*Job, error) {
	jobs, err := s.queryJobs(ctx, `WHERE j.norm_url = ? AND j.state NOT IN ('completed', 'cancelled')`, "ORDER BY j.id DESC LIMIT 1", []any{norm})
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, ErrNotFound
	}
	return jobs[0], nil
}

// OpenNormURLs returns the set of normalized URLs of open jobs.
func (s *Store) OpenNormURLs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT norm_url FROM jobs WHERE state NOT IN ('completed', 'cancelled')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}

// DeleteJob removes a job and its items.
func (s *Store) DeleteJob(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// JobIDs returns the IDs of jobs in the given states (all if none given).
func (s *Store) JobIDs(ctx context.Context, states ...JobState) ([]int64, error) {
	q := `SELECT id FROM jobs`
	var args []any
	if len(states) > 0 {
		ph := make([]string, len(states))
		for i, st := range states {
			ph[i] = "?"
			args = append(args, string(st))
		}
		q += ` WHERE state IN (` + strings.Join(ph, ",") + `)`
	}
	q += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q, args...)
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

// PrunableJobIDs returns completed jobs finished before t.
func (s *Store) PrunableJobIDs(ctx context.Context, before time.Time) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE state = 'completed' AND finished_at IS NOT NULL AND finished_at < ?`, before.UnixMilli())
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

// SetQueuePositions assigns queue positions in the given order, placed
// before all other queued jobs.
func (s *Store) SetQueuePositions(ctx context.Context, ids []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var minPos int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(queue_pos), 1) FROM jobs`).Scan(&minPos); err != nil {
		return err
	}
	start := minPos - int64(len(ids))
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET queue_pos = ? WHERE id = ?`, start+int64(i), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// NextQueuePos returns a position after all existing jobs.
func (s *Store) NextQueuePos(ctx context.Context) (int64, error) {
	var p int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(queue_pos), 0) + 1 FROM jobs`).Scan(&p)
	return p, err
}

// SetNextJobID makes sure new job IDs start after n (v1 migration).
func (s *Store) SetNextJobID(ctx context.Context, n int64) error {
	var cur sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'jobs'`).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.ExecContext(ctx, `INSERT INTO sqlite_sequence(name, seq) VALUES ('jobs', ?)`, n)
		return err
	}
	if err != nil {
		return err
	}
	if cur.Int64 < n {
		_, err = s.db.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = ? WHERE name = 'jobs'`, n)
	}
	return err
}
