package memory

import (
	"context"
	"database/sql"
	"errors"
)

// Preference is a user preference key/value.
type Preference struct {
	Key       string
	Value     string
	UpdatedAt Time
}

// SetPreference upserts a preference.
func (s *Store) SetPreference(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO preferences(key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, ts(s.now()))
	return err
}

// GetPreference returns the value and whether it exists.
func (s *Store) GetPreference(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM preferences WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// DeletePreference removes a preference.
func (s *Store) DeletePreference(ctx context.Context, key string) error {
	return rowsAffected(s.db.ExecContext(ctx, `DELETE FROM preferences WHERE key = ?`, key))
}

// ListPreferences returns all preferences ordered by key. Keys starting with
// "_" are internal (e.g. Telegram offsets) and are excluded unless internal.
func (s *Store) ListPreferences(ctx context.Context, internal bool) ([]Preference, error) {
	q := `SELECT key, value, updated_at FROM preferences WHERE substr(key, 1, 1) != '_' ORDER BY key`
	if internal {
		q = `SELECT key, value, updated_at FROM preferences ORDER BY key`
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Preference
	for rows.Next() {
		var p Preference
		var u string
		if err := rows.Scan(&p.Key, &p.Value, &u); err != nil {
			return nil, err
		}
		p.UpdatedAt = Time(parseTS(u))
		out = append(out, p)
	}
	return out, rows.Err()
}
