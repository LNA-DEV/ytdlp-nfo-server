package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/LNA-DEV/ytdlp-nfo-server/internal/config"
)

// --- yt-dlp versions ---

// UpsertVersion inserts or replaces an installed version.
func (s *Store) UpsertVersion(ctx context.Context, v *YtdlpVersion) error {
	if v.InstalledAt.IsZero() {
		v.InstalledAt = time.Now()
	}
	targets := v.ImpersonateTargets
	if targets == nil {
		targets = []string{}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO ytdlp_versions (id, channel, tag, version, path, dir, asset, sha256,
		bundled, pinned, smoke_ok, smoke_output, impersonate_targets, installed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET channel = excluded.channel, tag = excluded.tag, version = excluded.version,
		path = excluded.path, dir = excluded.dir, asset = excluded.asset, sha256 = excluded.sha256,
		bundled = excluded.bundled, pinned = excluded.pinned, smoke_ok = excluded.smoke_ok,
		smoke_output = excluded.smoke_output, impersonate_targets = excluded.impersonate_targets`,
		v.ID, v.Channel, v.Tag, v.Version, v.Path, v.Dir, v.Asset, v.SHA256, b2i(v.Bundled), b2i(v.Pinned),
		b2i(v.SmokeOK), v.SmokeOutput, mustJSON(targets), v.InstalledAt.UnixMilli())
	return err
}

// ListVersions returns installed versions, newest first.
func (s *Store) ListVersions(ctx context.Context) ([]*YtdlpVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, channel, tag, version, path, dir, asset, sha256, bundled, pinned,
		smoke_ok, smoke_output, impersonate_targets, installed_at FROM ytdlp_versions ORDER BY tag DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*YtdlpVersion
	for rows.Next() {
		var v YtdlpVersion
		var bundled, pinned, smoke int
		var targets string
		var inst int64
		if err := rows.Scan(&v.ID, &v.Channel, &v.Tag, &v.Version, &v.Path, &v.Dir, &v.Asset, &v.SHA256,
			&bundled, &pinned, &smoke, &v.SmokeOutput, &targets, &inst); err != nil {
			return nil, err
		}
		v.Bundled, v.Pinned, v.SmokeOK = bundled != 0, pinned != 0, smoke != 0
		_ = json.Unmarshal([]byte(targets), &v.ImpersonateTargets)
		v.InstalledAt = time.UnixMilli(inst)
		out = append(out, &v)
	}
	return out, rows.Err()
}

// GetVersion returns one installed version.
func (s *Store) GetVersion(ctx context.Context, id string) (*YtdlpVersion, error) {
	all, err := s.ListVersions(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range all {
		if v.ID == id {
			return v, nil
		}
	}
	return nil, ErrNotFound
}

// DeleteVersion removes a version record.
func (s *Store) DeleteVersion(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ytdlp_versions WHERE id = ?`, id)
	return err
}

// --- site rules ---

// ListSiteRules returns rules in match order.
func (s *Store) ListSiteRules(ctx context.Context) ([]config.SiteRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, position, data FROM site_rules ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []config.SiteRule{}
	for rows.Next() {
		var r config.SiteRule
		var id int64
		var pos int
		var data string
		if err := rows.Scan(&id, &pos, &data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			continue
		}
		r.ID, r.Position = id, pos
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveSiteRule inserts (ID 0) or updates a rule.
func (s *Store) SaveSiteRule(ctx context.Context, r *config.SiteRule) error {
	if r.ID == 0 {
		if r.Position == 0 {
			if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM site_rules`).Scan(&r.Position); err != nil {
				return err
			}
		}
		res, err := s.db.ExecContext(ctx, `INSERT INTO site_rules (position, data) VALUES (?, ?)`, r.Position, mustJSON(r))
		if err != nil {
			return err
		}
		r.ID, err = res.LastInsertId()
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE site_rules SET position = ?, data = ? WHERE id = ?`, r.Position, mustJSON(r), r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSiteRule removes a rule.
func (s *Store) DeleteSiteRule(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM site_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- cookie profiles ---

// ListCookieProfiles returns all profiles.
func (s *Store) ListCookieProfiles(ctx context.Context) ([]CookieProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, filename, size, created_at FROM cookie_profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CookieProfile{}
	for rows.Next() {
		var c CookieProfile
		var created int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Filename, &c.Size, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = time.UnixMilli(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCookieProfile returns a profile by name.
func (s *Store) GetCookieProfile(ctx context.Context, name string) (*CookieProfile, error) {
	var c CookieProfile
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, name, filename, size, created_at FROM cookie_profiles WHERE name = ?`, name).
		Scan(&c.ID, &c.Name, &c.Filename, &c.Size, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt = time.UnixMilli(created)
	return &c, nil
}

// SaveCookieProfile inserts or replaces a profile by name.
func (s *Store) SaveCookieProfile(ctx context.Context, c *CookieProfile) error {
	c.CreatedAt = time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO cookie_profiles (name, filename, size, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET filename = excluded.filename, size = excluded.size, created_at = excluded.created_at`,
		c.Name, c.Filename, c.Size, c.CreatedAt.UnixMilli())
	if err != nil {
		return err
	}
	c.ID, _ = res.LastInsertId()
	return nil
}

// DeleteCookieProfile removes a profile.
func (s *Store) DeleteCookieProfile(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM cookie_profiles WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- API tokens ---

// CreateToken stores a token hash.
func (s *Store) CreateToken(ctx context.Context, t *APIToken) error {
	t.CreatedAt = time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens (name, hash, prefix, created_at) VALUES (?, ?, ?, ?)`,
		t.Name, t.Hash, t.Prefix, t.CreatedAt.UnixMilli())
	if err != nil {
		return err
	}
	t.ID, err = res.LastInsertId()
	return err
}

// ListTokens returns all tokens (without hashes).
func (s *Store) ListTokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, prefix, created_at, last_used_at FROM api_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		var t APIToken
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &created, &used); err != nil {
			return nil, err
		}
		t.CreatedAt = time.UnixMilli(created)
		t.LastUsedAt = fromMs(used)
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenByHash looks up a token and records its use.
func (s *Store) TokenByHash(ctx context.Context, hash string) (*APIToken, error) {
	var t APIToken
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, name, prefix, created_at FROM api_tokens WHERE hash = ?`, hash).
		Scan(&t.ID, &t.Name, &t.Prefix, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = time.UnixMilli(created)
	_, _ = s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, time.Now().UnixMilli(), t.ID)
	return &t, nil
}

// DeleteToken revokes a token.
func (s *Store) DeleteToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- sessions ---

// CreateSession stores a session hash.
func (s *Store) CreateSession(ctx context.Context, hash string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (hash, created_at, expires_at) VALUES (?, ?, ?)`,
		hash, time.Now().UnixMilli(), expires.UnixMilli())
	return err
}

// SessionValid reports whether a session exists and is not expired, and
// extends it (sliding expiry).
func (s *Store) SessionValid(ctx context.Context, hash string, extend time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE hash = ? AND expires_at > ?`,
		extend.UnixMilli(), hash, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE hash = ?`, hash)
	return err
}

// DeleteAllSessions logs everyone out (password change).
func (s *Store) DeleteAllSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions`)
	return err
}

// PruneSessions deletes expired sessions.
func (s *Store) PruneSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, time.Now().UnixMilli())
	return err
}
