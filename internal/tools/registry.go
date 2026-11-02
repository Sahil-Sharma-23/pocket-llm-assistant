// Package tools defines the strongly typed tool registry used by agents.
//
// A tool's permission level is part of its Go definition. Nothing produced by
// the LLM can change it: the model only proposes a tool name and JSON
// arguments, and the agent executor looks the permission up here.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Permission levels.
type Permission string

const (
	// Read tools execute without confirmation.
	Read Permission = "read"
	// Write tools change state; consequential calls need approval.
	Write Permission = "write"
	// Sensitive tools always need explicit user approval.
	Sensitive Permission = "sensitive"
)

// RetryPolicy controls re-execution of transient failures. Attempts counts
// the first try (1 = no retry). Only errors wrapped with Retryable are retried.
type RetryPolicy struct {
	Attempts int
	Backoff  time.Duration
}

// ErrInvalidArgs wraps argument decoding/validation failures.
var ErrInvalidArgs = errors.New("invalid tool arguments")

// retryableError marks a transient failure.
type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

// Retryable marks err as transient (network timeouts, 5xx...).
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{err}
}

// IsRetryable reports whether err was marked Retryable.
func IsRetryable(err error) bool {
	var r retryableError
	return errors.As(err, &r)
}

// Validator is implemented by tool input structs with semantic checks.
type Validator interface{ Validate() error }

// Tool is a registered, executable capability.
type Tool struct {
	Name        string
	Description string
	// Schema is the JSON Schema of the arguments object. It documents the
	// tool for the model and constrains its output via llama.cpp grammars.
	Schema     json.RawMessage
	Permission Permission
	Timeout    time.Duration
	Retry      RetryPolicy

	decode        func(json.RawMessage) (any, error)
	run           func(context.Context, any) (any, error)
	consequential func(any) bool
	summarize     func(any) string
	confirm       func(in, out any) string
}

// Spec describes a tool with typed input In and output Out.
type Spec[In, Out any] struct {
	Name        string
	Description string
	Schema      string
	Permission  Permission
	Timeout     time.Duration
	Retry       RetryPolicy
	Run         func(ctx context.Context, in In) (Out, error)
	// Consequential decides, for Write tools, whether this particular call
	// needs approval. nil = never (Write) / always (Sensitive).
	Consequential func(in In) bool
	// Summarize renders the call for humans (approval prompts). Optional.
	Summarize func(in In) string
	// Confirm renders a deterministic user-facing confirmation after a
	// successful call. When set, the agent run ends with this text instead of
	// spending another LLM call (~25 s on a mid-range reference phone) on wording.
	Confirm func(in In, out Out) string
}

// New builds a Tool from a typed Spec.
func New[In, Out any](s Spec[In, Out]) Tool {
	t := Tool{
		Name:        s.Name,
		Description: s.Description,
		Schema:      json.RawMessage(s.Schema),
		Permission:  s.Permission,
		Timeout:     s.Timeout,
		Retry:       s.Retry,
		decode: func(raw json.RawMessage) (any, error) {
			var in In
			if len(bytes.TrimSpace(raw)) == 0 {
				raw = json.RawMessage("{}")
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&in); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidArgs, err)
			}
			if v, ok := any(&in).(Validator); ok {
				if err := v.Validate(); err != nil {
					return nil, fmt.Errorf("%w: %v", ErrInvalidArgs, err)
				}
			}
			return in, nil
		},
		run: func(ctx context.Context, in any) (any, error) { return s.Run(ctx, in.(In)) },
	}
	if s.Consequential != nil {
		t.consequential = func(in any) bool { return s.Consequential(in.(In)) }
	}
	if s.Summarize != nil {
		t.summarize = func(in any) string { return s.Summarize(in.(In)) }
	}
	if s.Confirm != nil {
		t.confirm = func(in, out any) string { return s.Confirm(in.(In), out.(Out)) }
	}
	return t
}

// Call is a decoded, validated invocation ready to execute.
type Call struct {
	Tool  Tool
	Args  json.RawMessage // canonical (re-encoded) arguments
	input any
}

// Prepare decodes and validates raw arguments.
func (t Tool) Prepare(raw json.RawMessage) (Call, error) {
	in, err := t.decode(raw)
	if err != nil {
		return Call{}, err
	}
	canon, err := json.Marshal(in)
	if err != nil {
		return Call{}, err
	}
	return Call{Tool: t, Args: canon, input: in}, nil
}

// NeedsApproval applies the permission policy to this call.
func (c Call) NeedsApproval() bool {
	switch c.Tool.Permission {
	case Sensitive:
		return true
	case Write:
		return c.Tool.consequential != nil && c.Tool.consequential(c.input)
	default:
		return false
	}
}

// Summary describes the call for a human.
func (c Call) Summary() string {
	if c.Tool.summarize != nil {
		return c.Tool.summarize(c.input)
	}
	return fmt.Sprintf("%s %s", c.Tool.Name, c.Args)
}

// Result is the outcome of Execute.
type Result struct {
	Output       json.RawMessage
	Confirmation string // set when the tool defines Confirm
	Attempts     int
	Duration     time.Duration
}

// Execute runs the call with its timeout and retry policy. Permission
// enforcement is the caller's job (agent.Executor); Execute only runs.
func (c Call) Execute(ctx context.Context) (Result, error) {
	attempts := max(c.Tool.Retry.Attempts, 1)
	start := time.Now()
	var lastErr error
	for i := 1; i <= attempts; i++ {
		out, err := c.once(ctx)
		if err == nil {
			raw, merr := json.Marshal(out)
			if merr != nil {
				return Result{Attempts: i, Duration: time.Since(start)}, fmt.Errorf("encode result: %w", merr)
			}
			res := Result{Output: raw, Attempts: i, Duration: time.Since(start)}
			if c.Tool.confirm != nil {
				res.Confirmation = c.Tool.confirm(c.input, out)
			}
			return res, nil
		}
		lastErr = err
		if !IsRetryable(err) || i == attempts || ctx.Err() != nil {
			return Result{Attempts: i, Duration: time.Since(start)}, err
		}
		select {
		case <-ctx.Done():
			return Result{Attempts: i, Duration: time.Since(start)}, ctx.Err()
		case <-time.After(c.Tool.Retry.Backoff * time.Duration(i)):
		}
	}
	return Result{Attempts: attempts, Duration: time.Since(start)}, lastErr
}

func (c Call) once(ctx context.Context) (out any, err error) {
	if c.Tool.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Tool.Timeout)
		defer cancel()
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("tool %s panicked: %v", c.Tool.Name, p)
		}
	}()
	return c.Tool.run(ctx, c.input)
}

// Registry holds tools by name.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

var toolName = regexp.MustCompile(`^[a-z][a-z0-9_]{1,40}$`)

// Register validates and adds a tool.
func (r *Registry) Register(t Tool) error {
	switch {
	case !toolName.MatchString(t.Name):
		return fmt.Errorf("tool %q: invalid name", t.Name)
	case r.tools[t.Name].Name != "":
		return fmt.Errorf("tool %q: already registered", t.Name)
	case t.Description == "":
		return fmt.Errorf("tool %q: description required", t.Name)
	case !json.Valid(t.Schema):
		return fmt.Errorf("tool %q: schema is not valid JSON", t.Name)
	case t.Permission != Read && t.Permission != Write && t.Permission != Sensitive:
		return fmt.Errorf("tool %q: invalid permission %q", t.Name, t.Permission)
	case t.Timeout <= 0:
		return fmt.Errorf("tool %q: timeout required", t.Name)
	case t.decode == nil || t.run == nil:
		return fmt.Errorf("tool %q: construct with tools.New", t.Name)
	}
	r.tools[t.Name] = t
	return nil
}

// MustRegister panics on error (startup wiring only).
func (r *Registry) MustRegister(ts ...Tool) {
	for _, t := range ts {
		if err := r.Register(t); err != nil {
			panic(err)
		}
	}
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// List returns all tools sorted by name.
func (r *Registry) List() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
