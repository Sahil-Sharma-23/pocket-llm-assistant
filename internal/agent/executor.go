package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"personal-assistant/internal/memory"
	"personal-assistant/internal/tools"
)

// Outcome statuses of a proposed tool call.
const (
	OutcomeOK               = "ok"
	OutcomeError            = "error"
	OutcomeInvalid          = "invalid"
	OutcomeDenied           = "denied"
	OutcomeAwaitingApproval = "awaiting_approval"
)

// Outcome is the result of proposing a tool call.
type Outcome struct {
	Status   string
	Output   json.RawMessage
	Error    string
	Approval *memory.Approval
	// Confirmation is a deterministic user-facing message from the tool.
	Confirmation string
}

// ForModel renders the outcome as the tool result shown to the LLM.
func (o Outcome) ForModel() string {
	switch o.Status {
	case OutcomeOK:
		return string(o.Output)
	case OutcomeAwaitingApproval:
		return fmt.Sprintf(`{"status":"awaiting_user_approval","approval_id":%d}`, o.Approval.ID)
	default:
		return "ERROR: " + o.Error
	}
}

// Origin identifies where a request came from (for approval notifications).
type Origin struct {
	Channel    string
	ExternalID string
}

// Executor is the only path from a model/command proposal to tool execution.
// It enforces, independent of anything the model says:
//   - the agent may only call tools in its allowed list;
//   - arguments are strictly decoded and validated;
//   - the permission level comes from the tool registry, and Sensitive or
//     consequential Write calls become approval requests instead of running;
//   - approved actions execute at most once.
type Executor struct {
	Tools       *tools.Registry
	Store       *memory.Store
	Log         *slog.Logger
	ApprovalTTL time.Duration

	mu        sync.Mutex
	executing map[int64]bool
}

// Propose validates and either executes a call or files an approval request.
func (e *Executor) Propose(ctx context.Context, runID int64, origin Origin, allowed []string, name string, rawArgs json.RawMessage) Outcome {
	tool, ok := e.Tools.Get(name)
	if !ok || (allowed != nil && !slices.Contains(allowed, name)) {
		out := Outcome{Status: OutcomeDenied, Error: fmt.Sprintf("tool %q is not available to this agent", name)}
		e.record(ctx, runID, name, "read", rawArgs, out, 0, 0)
		return out
	}
	call, err := tool.Prepare(rawArgs)
	if err != nil {
		out := Outcome{Status: OutcomeInvalid, Error: err.Error()}
		e.record(ctx, runID, name, string(tool.Permission), rawArgs, out, 0, 0)
		return out
	}

	if call.NeedsApproval() {
		a, err := e.Store.CreateApproval(ctx, memory.NewApproval{
			RunID: runID, Tool: name, Permission: string(tool.Permission), Args: string(call.Args),
			Summary: call.Summary(), Channel: origin.Channel, ExternalID: origin.ExternalID, TTL: e.ApprovalTTL,
		})
		if err != nil {
			out := Outcome{Status: OutcomeError, Error: "could not create approval request: " + err.Error()}
			e.record(ctx, runID, name, string(tool.Permission), call.Args, out, 0, 0)
			return out
		}
		out := Outcome{Status: OutcomeAwaitingApproval, Approval: &a}
		e.record(ctx, runID, name, string(tool.Permission), call.Args, out, 0, 0)
		e.Log.Info("approval requested", "approval_id", a.ID, "tool", name, "permission", tool.Permission)
		return out
	}

	res, err := call.Execute(ctx)
	out := Outcome{Status: OutcomeOK, Output: res.Output, Confirmation: res.Confirmation}
	if err != nil {
		out = Outcome{Status: OutcomeError, Error: err.Error()}
	}
	e.record(ctx, runID, name, string(tool.Permission), call.Args, out, res.Attempts, res.Duration)
	return out
}

// ErrAlreadyExecuting guards against double execution within the process.
var ErrAlreadyExecuting = errors.New("approval is already being executed")

// ExecuteApproved runs an approval that is in status "approved". The store
// transition approved -> executed|failed happens exactly once.
func (e *Executor) ExecuteApproved(ctx context.Context, id int64) (memory.Approval, Outcome, error) {
	e.mu.Lock()
	if e.executing == nil {
		e.executing = map[int64]bool{}
	}
	if e.executing[id] {
		e.mu.Unlock()
		return memory.Approval{}, Outcome{}, ErrAlreadyExecuting
	}
	e.executing[id] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.executing, id)
		e.mu.Unlock()
	}()

	a, err := e.Store.GetApproval(ctx, id)
	if err != nil {
		return memory.Approval{}, Outcome{}, err
	}
	if a.Status != memory.ApprovalApproved {
		return a, Outcome{}, memory.ErrApprovalNotPending
	}
	tool, ok := e.Tools.Get(a.Tool)
	var out Outcome
	var res tools.Result
	if !ok {
		out = Outcome{Status: OutcomeError, Error: "tool no longer exists: " + a.Tool}
	} else if call, perr := tool.Prepare(json.RawMessage(a.Args)); perr != nil {
		out = Outcome{Status: OutcomeInvalid, Error: perr.Error()}
	} else {
		res, err = call.Execute(ctx)
		out = Outcome{Status: OutcomeOK, Output: res.Output}
		if err != nil {
			out = Outcome{Status: OutcomeError, Error: err.Error()}
		}
	}
	var execErr error
	if out.Status != OutcomeOK {
		execErr = errors.New(out.Error)
	}
	if err := e.Store.FinishApproval(ctx, id, string(out.Output), execErr); err != nil {
		return a, out, err
	}
	e.record(ctx, a.RunID, a.Tool, a.Permission, json.RawMessage(a.Args), out, res.Attempts, res.Duration)
	e.Log.Info("approved action executed", "approval_id", id, "tool", a.Tool, "status", out.Status)
	a, _ = e.Store.GetApproval(ctx, id)
	return a, out, nil
}

func (e *Executor) record(ctx context.Context, runID int64, tool, perm string, args json.RawMessage, out Outcome, attempts int, d time.Duration) {
	status := out.Status
	if status == OutcomeDenied {
		status = OutcomeInvalid
	}
	if _, err := e.Store.AddToolCall(ctx, memory.ToolCall{
		RunID: runID, Tool: tool, Permission: perm, Args: string(args), Result: string(out.Output),
		Status: status, Error: out.Error, Attempts: attempts, DurationMS: d.Milliseconds(),
	}); err != nil {
		e.Log.Warn("record tool call", "err", err)
	}
}
