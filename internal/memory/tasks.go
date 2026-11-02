package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Task statuses.
const (
	TaskPending   = "pending"
	TaskDone      = "done"
	TaskCancelled = "cancelled"
)

// Task is a to-do item.
type Task struct {
	ID          int64
	Title       string
	Notes       string
	Status      string
	Priority    int // 1 high, 2 normal, 3 low
	DueAt       *time.Time
	CreatedAt   Time
	UpdatedAt   Time
	CompletedAt *time.Time
}

// NewTask is the input to CreateTask.
type NewTask struct {
	Title    string
	Notes    string
	Priority int
	DueAt    *time.Time
}

// CreateTask inserts a pending task.
func (s *Store) CreateTask(ctx context.Context, in NewTask) (Task, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return Task{}, errors.New("task: title is required")
	}
	if in.Priority == 0 {
		in.Priority = 2
	}
	if in.Priority < 1 || in.Priority > 3 {
		return Task{}, fmt.Errorf("task: priority must be 1-3, got %d", in.Priority)
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO tasks(title, notes, priority, due_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, in.Title, in.Notes, in.Priority, nullTS(in.DueAt), ts(now), ts(now))
	if err != nil {
		return Task{}, fmt.Errorf("insert task: %w", err)
	}
	id, _ := res.LastInsertId()
	return s.GetTask(ctx, id)
}

// GetTask loads one task.
func (s *Store) GetTask(ctx context.Context, id int64) (Task, error) {
	rows, err := s.db.QueryContext(ctx, taskSelect+` WHERE id = ?`, id)
	if err != nil {
		return Task{}, err
	}
	defer rows.Close()
	list, err := scanTasks(rows)
	if err != nil {
		return Task{}, err
	}
	if len(list) == 0 {
		return Task{}, ErrNotFound
	}
	return list[0], nil
}

// ListTasks lists tasks with the given status ("" = all), pending first by
// priority then due date.
func (s *Store) ListTasks(ctx context.Context, status string, limit int) ([]Task, error) {
	q := taskSelect
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY status = 'pending' DESC, priority, due_at IS NULL, due_at, id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// SetTaskStatus moves a task to done/cancelled/pending.
func (s *Store) SetTaskStatus(ctx context.Context, id int64, status string) (Task, error) {
	switch status {
	case TaskPending, TaskDone, TaskCancelled:
	default:
		return Task{}, fmt.Errorf("task: invalid status %q", status)
	}
	now := s.now()
	var completed any
	if status == TaskDone {
		completed = ts(now)
	}
	if err := rowsAffected(s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, completed_at = ?, updated_at = ? WHERE id = ?`,
		status, completed, ts(now), id)); err != nil {
		return Task{}, err
	}
	return s.GetTask(ctx, id)
}

// DeleteTask permanently removes a task.
func (s *Store) DeleteTask(ctx context.Context, id int64) error {
	return rowsAffected(s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id))
}

const taskSelect = `SELECT id, title, notes, status, priority, due_at, created_at, updated_at, completed_at FROM tasks`

func scanTasks(rows *sql.Rows) ([]Task, error) {
	var out []Task
	for rows.Next() {
		var t Task
		var due, done sql.NullString
		var c, u string
		if err := rows.Scan(&t.ID, &t.Title, &t.Notes, &t.Status, &t.Priority, &due, &c, &u, &done); err != nil {
			return nil, err
		}
		t.DueAt, t.CompletedAt = parseNullTS(due), parseNullTS(done)
		t.CreatedAt, t.UpdatedAt = Time(parseTS(c)), Time(parseTS(u))
		out = append(out, t)
	}
	return out, rows.Err()
}
