package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Approval statuses. Transitions are enforced in SQL so concurrent decisions
// (e.g. Telegram button + dashboard click) cannot both win:
//
//	pending -> approved -> executed | failed
//	pending -> rejected | expired
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalRejected = "rejected"
	ApprovalExpired  = "expired"
	ApprovalExecuted = "executed"
	ApprovalFailed   = "failed"
)

// ErrApprovalNotPending means the approval was already decided or expired.
var ErrApprovalNotPending = errors.New("approval is no longer pending")

// Approval is a request for the user to authorise a tool call.
type Approval struct {
	ID         int64
	RunID      int64
	Tool       string
	Permission string
	Args       string
	Summary    string
	Channel    string
	ExternalID string
	Status     string
	DecidedBy  string
	Result     string
	Error      string
	CreatedAt  Time
	ExpiresAt  Time
	DecidedAt  *time.Time
	ExecutedAt *time.Time
}

// NewApproval is the input to CreateApproval.
type NewApproval struct {
	RunID      int64
	Tool       string
	Permission string
	Args       string
	Summary    string
	Channel    string
	ExternalID string
	TTL        time.Duration
}

// CreateApproval stores a pending approval.
func (s *Store) CreateApproval(ctx context.Context, in NewApproval) (Approval, error) {
	if in.Permission != "write" && in.Permission != "sensitive" {
		return Approval{}, fmt.Errorf("approval: invalid permission %q", in.Permission)
	}
	now := s.now()
	var run any
	if in.RunID > 0 {
		run = in.RunID
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO approvals(run_id, tool, permission, args, summary, channel, external_id,
		status, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		run, in.Tool, in.Permission, in.Args, in.Summary, in.Channel, in.ExternalID, ts(now), ts(now.Add(in.TTL)))
	if err != nil {
		return Approval{}, fmt.Errorf("insert approval: %w", err)
	}
	id, _ := res.LastInsertId()
	return s.GetApproval(ctx, id)
}

// GetApproval loads one approval.
func (s *Store) GetApproval(ctx context.Context, id int64) (Approval, error) {
	rows, err := s.db.QueryContext(ctx, approvalSelect+` WHERE id = ?`, id)
	if err != nil {
		return Approval{}, err
	}
	defer rows.Close()
	list, err := scanApprovals(rows)
	if err != nil {
		return Approval{}, err
	}
	if len(list) == 0 {
		return Approval{}, ErrNotFound
	}
	return list[0], nil
}

// ListApprovals lists approvals by status ("" = all), newest first.
func (s *Store) ListApprovals(ctx context.Context, status string, limit int) ([]Approval, error) {
	q, args := approvalSelect, []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanApprovals(rows)
}

// DecideApproval atomically moves a pending, unexpired approval to approved
// or rejected. It returns ErrApprovalNotPending if someone else decided first
// or it expired.
func (s *Store) DecideApproval(ctx context.Context, id int64, approve bool, by string) (Approval, error) {
	status := ApprovalRejected
	if approve {
		status = ApprovalApproved
	}
	now := ts(s.now())
	err := rowsAffected(s.db.ExecContext(ctx, `UPDATE approvals SET status = ?, decided_by = ?, decided_at = ?
		WHERE id = ? AND status = 'pending' AND expires_at > ?`, status, by, now, id, now))
	if errors.Is(err, ErrNotFound) {
		if _, gerr := s.GetApproval(ctx, id); gerr != nil {
			return Approval{}, gerr
		}
		s.ExpireApprovals(ctx)
		return Approval{}, ErrApprovalNotPending
	}
	if err != nil {
		return Approval{}, err
	}
	return s.GetApproval(ctx, id)
}

// FinishApproval records the execution result of an approved action. Only
// approved rows can transition, which guarantees at-most-once execution.
func (s *Store) FinishApproval(ctx context.Context, id int64, result string, execErr error) error {
	status, msg := ApprovalExecuted, ""
	if execErr != nil {
		status, msg = ApprovalFailed, execErr.Error()
	}
	err := rowsAffected(s.db.ExecContext(ctx, `UPDATE approvals SET status = ?, result = ?, error = ?, executed_at = ?
		WHERE id = ? AND status = 'approved'`, status, result, msg, ts(s.now()), id))
	if errors.Is(err, ErrNotFound) {
		return ErrApprovalNotPending
	}
	return err
}

// MarkInterruptedApprovals fails approvals that were approved but never
// finished (the process died mid-execution). They are not re-run
// automatically: the user must ask again, which keeps execution at most once.
func (s *Store) MarkInterruptedApprovals(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status = 'failed',
		error = 'interrupted: process restarted before execution finished', executed_at = ?
		WHERE status = 'approved'`, ts(s.now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ExpireApprovals marks overdue pending approvals as expired.
func (s *Store) ExpireApprovals(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE approvals SET status = 'expired' WHERE status = 'pending' AND expires_at <= ?`, ts(s.now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const approvalSelect = `SELECT id, COALESCE(run_id, 0), tool, permission, args, summary, channel, external_id, status,
	decided_by, result, error, created_at, expires_at, decided_at, executed_at FROM approvals`

func scanApprovals(rows *sql.Rows) ([]Approval, error) {
	var out []Approval
	for rows.Next() {
		var a Approval
		var c, e string
		var d, x sql.NullString
		if err := rows.Scan(&a.ID, &a.RunID, &a.Tool, &a.Permission, &a.Args, &a.Summary, &a.Channel, &a.ExternalID,
			&a.Status, &a.DecidedBy, &a.Result, &a.Error, &c, &e, &d, &x); err != nil {
			return nil, err
		}
		a.CreatedAt, a.ExpiresAt = Time(parseTS(c)), Time(parseTS(e))
		a.DecidedAt, a.ExecutedAt = parseNullTS(d), parseNullTS(x)
		out = append(out, a)
	}
	return out, rows.Err()
}
