package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Job is a scheduled background job.
type Job struct {
	ID         int64
	Kind       string
	Name       string
	Payload    string
	Schedule   string
	NextRunAt  time.Time
	LastRunAt  *time.Time
	LastStatus string
	LastError  string
	Enabled    bool
	CreatedAt  Time
}

// JobSpec is the input to UpsertJob / CreateJob.
type JobSpec struct {
	Kind, Name, Payload, Schedule string
	NextRunAt                     time.Time
}

// CreateJob inserts a new job; it fails if (kind, name) exists.
func (s *Store) CreateJob(ctx context.Context, j JobSpec) (Job, error) {
	if j.Payload == "" {
		j.Payload = "{}"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs(kind, name, payload, schedule, next_run_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, j.Kind, j.Name, j.Payload, j.Schedule, ts(j.NextRunAt), ts(s.now()))
	if err != nil {
		return Job{}, fmt.Errorf("insert job: %w", err)
	}
	id, _ := res.LastInsertId()
	return s.GetJob(ctx, id)
}

// UpsertJob creates a config-defined job or updates its payload/schedule.
// next_run_at is only reset when the schedule changed, so restarts do not
// re-trigger jobs.
func (s *Store) UpsertJob(ctx context.Context, j JobSpec) error {
	if j.Payload == "" {
		j.Payload = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs(kind, name, payload, schedule, next_run_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, name) DO UPDATE SET
			payload = excluded.payload,
			next_run_at = CASE WHEN jobs.schedule != excluded.schedule THEN excluded.next_run_at ELSE jobs.next_run_at END,
			schedule = excluded.schedule,
			enabled = 1`,
		j.Kind, j.Name, j.Payload, j.Schedule, ts(j.NextRunAt), ts(s.now()))
	return err
}

// DisableJobsExcept disables jobs of kind whose name is not in keep (e.g. a
// monitor removed from config).
func (s *Store) DisableJobsExcept(ctx context.Context, kind string, keep []string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM jobs WHERE kind = ? AND enabled = 1`, kind)
	if err != nil {
		return err
	}
	keepSet := map[string]bool{}
	for _, k := range keep {
		keepSet[k] = true
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		if !keepSet[name] {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET enabled = 0 WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// GetJob loads one job.
func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE id = ?`, id)
	if err != nil {
		return Job{}, err
	}
	defer rows.Close()
	list, err := scanJobs(rows)
	if err != nil {
		return Job{}, err
	}
	if len(list) == 0 {
		return Job{}, ErrNotFound
	}
	return list[0], nil
}

// ListJobs lists jobs (enabled first, by next run).
func (s *Store) ListJobs(ctx context.Context, kind string, limit int) ([]Job, error) {
	q, args := jobSelect, []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY enabled DESC, next_run_at LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// DueJobs returns enabled jobs whose next_run_at <= now.
func (s *Store) DueJobs(ctx context.Context, now time.Time, limit int) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, jobSelect+` WHERE enabled = 1 AND next_run_at <= ? ORDER BY next_run_at LIMIT ?`, ts(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// NextDueAt returns the earliest next_run_at of enabled jobs, or nil.
func (s *Store) NextDueAt(ctx context.Context) (*time.Time, error) {
	var v sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT min(next_run_at) FROM jobs WHERE enabled = 1`).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return parseNullTS(v), nil
}

// RecordJobRun stores the result of a run and schedules the next one; a nil
// next disables the job (one-shot jobs).
func (s *Store) RecordJobRun(ctx context.Context, id int64, ranAt time.Time, runErr error, next *time.Time) error {
	status, msg := "ok", ""
	if runErr != nil {
		status, msg = "error", runErr.Error()
	}
	enabled, nextTS := 1, ts(ranAt)
	if next == nil {
		enabled = 0
	} else {
		nextTS = ts(*next)
	}
	return rowsAffected(s.db.ExecContext(ctx, `UPDATE jobs SET last_run_at = ?, last_status = ?, last_error = ?,
		next_run_at = ?, enabled = ? WHERE id = ?`, ts(ranAt), status, msg, nextTS, enabled, id))
}

// DeleteJob removes a job.
func (s *Store) DeleteJob(ctx context.Context, id int64) error {
	return rowsAffected(s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id))
}

const jobSelect = `SELECT id, kind, name, payload, schedule, next_run_at, last_run_at, last_status, last_error,
	enabled, created_at FROM jobs`

func scanJobs(rows *sql.Rows) ([]Job, error) {
	var out []Job
	for rows.Next() {
		var j Job
		var next, created string
		var last sql.NullString
		var enabled int
		if err := rows.Scan(&j.ID, &j.Kind, &j.Name, &j.Payload, &j.Schedule, &next, &last, &j.LastStatus,
			&j.LastError, &enabled, &created); err != nil {
			return nil, err
		}
		j.NextRunAt, j.LastRunAt = parseTS(next), parseNullTS(last)
		j.Enabled, j.CreatedAt = enabled == 1, Time(parseTS(created))
		out = append(out, j)
	}
	return out, rows.Err()
}
