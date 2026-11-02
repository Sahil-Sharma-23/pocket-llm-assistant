package memory

import (
	"context"
	"database/sql"
	"errors"
)

// Run statuses.
const (
	RunRunning          = "running"
	RunOK               = "ok"
	RunError            = "error"
	RunAwaitingApproval = "awaiting_approval"
)

// Run is one agent execution record.
type Run struct {
	ID               int64
	ConversationID   int64 // 0 = none
	Agent            string
	Trigger          string
	Input            string
	Output           string
	Status           string
	Error            string
	LLMCalls         int
	PromptTokens     int
	CompletionTokens int
	StartedAt        Time
	FinishedAt       *Time
}

// Duration returns the run time in milliseconds (0 while running).
func (r Run) DurationMS() int64 {
	if r.FinishedAt == nil {
		return 0
	}
	return r.FinishedAt.T().Sub(r.StartedAt.T()).Milliseconds()
}

// ToolCall records a single tool invocation.
type ToolCall struct {
	ID         int64
	RunID      int64
	Tool       string
	Permission string
	Args       string
	Result     string
	Status     string
	Error      string
	Attempts   int
	DurationMS int64
	CreatedAt  Time
}

// RunUsage carries LLM usage counters when finishing a run.
type RunUsage struct {
	LLMCalls, PromptTokens, CompletionTokens int
}

// StartRun records a new running execution.
func (s *Store) StartRun(ctx context.Context, conversationID int64, agent, trigger, input string) (int64, error) {
	var conv any
	if conversationID > 0 {
		conv = conversationID
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO agent_runs(conversation_id, agent, trigger, input, status, started_at)
		VALUES (?, ?, ?, ?, 'running', ?)`, conv, agent, trigger, input, ts(s.now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishRun stores the outcome of a run.
func (s *Store) FinishRun(ctx context.Context, id int64, status, output, errMsg string, u RunUsage) error {
	return rowsAffected(s.db.ExecContext(ctx, `UPDATE agent_runs SET status = ?, output = ?, error = ?,
		llm_calls = ?, prompt_tokens = ?, completion_tokens = ?, finished_at = ? WHERE id = ?`,
		status, output, errMsg, u.LLMCalls, u.PromptTokens, u.CompletionTokens, ts(s.now()), id))
}

// AddToolCall records a tool invocation.
func (s *Store) AddToolCall(ctx context.Context, tc ToolCall) (int64, error) {
	var run any
	if tc.RunID > 0 {
		run = tc.RunID
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO tool_calls(run_id, tool, permission, args, result, status, error, attempts, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, run, tc.Tool, tc.Permission, tc.Args, tc.Result, tc.Status,
		tc.Error, tc.Attempts, tc.DurationMS, ts(s.now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListRuns returns the newest runs.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelect+` ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

// GetRun loads one run.
func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelect+` WHERE id = ?`, id)
	if err != nil {
		return Run{}, err
	}
	defer rows.Close()
	list, err := scanRuns(rows)
	if err != nil {
		return Run{}, err
	}
	if len(list) == 0 {
		return Run{}, ErrNotFound
	}
	return list[0], nil
}

// ListToolCalls returns the tool calls of a run in order.
func (s *Store) ListToolCalls(ctx context.Context, runID int64) ([]ToolCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(run_id, 0), tool, permission, args, result, status, error,
		attempts, duration_ms, created_at FROM tool_calls WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolCall
	for rows.Next() {
		var t ToolCall
		var c string
		if err := rows.Scan(&t.ID, &t.RunID, &t.Tool, &t.Permission, &t.Args, &t.Result, &t.Status, &t.Error,
			&t.Attempts, &t.DurationMS, &c); err != nil {
			return nil, err
		}
		t.CreatedAt = Time(parseTS(c))
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkStaleRuns marks runs left "running" by a crash as errors. Called at
// startup: Android may SIGKILL the process mid-run.
func (s *Store) MarkStaleRuns(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE agent_runs SET status = 'error',
		error = 'interrupted: process restarted', finished_at = ? WHERE status = 'running'`, ts(s.now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const runSelect = `SELECT id, COALESCE(conversation_id, 0), agent, trigger, input, output, status, error,
	llm_calls, prompt_tokens, completion_tokens, started_at, finished_at FROM agent_runs`

func scanRuns(rows *sql.Rows) ([]Run, error) {
	var out []Run
	for rows.Next() {
		var r Run
		var started string
		var finished sql.NullString
		if err := rows.Scan(&r.ID, &r.ConversationID, &r.Agent, &r.Trigger, &r.Input, &r.Output, &r.Status,
			&r.Error, &r.LLMCalls, &r.PromptTokens, &r.CompletionTokens, &started, &finished); err != nil {
			return nil, err
		}
		r.StartedAt = Time(parseTS(started))
		if f := parseNullTS(finished); f != nil {
			ft := Time(*f)
			r.FinishedAt = &ft
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return out, nil
}
