package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"personal-assistant/internal/memory"
)

// TaskView is the model-facing task shape (local times as strings).
type TaskView struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
	Due      string `json:"due,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

func taskView(t memory.Task, loc *time.Location) TaskView {
	v := TaskView{ID: t.ID, Title: t.Title, Status: t.Status, Priority: t.Priority, Notes: t.Notes}
	if t.DueAt != nil {
		v.Due = FormatLocal(*t.DueAt, loc)
	}
	return v
}

// --- task_create -------------------------------------------------------------

type TaskCreateIn struct {
	Title    string `json:"title"`
	Notes    string `json:"notes,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Due      string `json:"due,omitempty"`
}

func (in *TaskCreateIn) Validate() error {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len(in.Title) > 200 {
		return errors.New("title must be 1-200 characters")
	}
	if in.Priority != 0 && (in.Priority < 1 || in.Priority > 3) {
		return errors.New("priority must be 1 (high), 2 or 3 (low)")
	}
	return nil
}

func TaskCreate(d Deps) Tool {
	return New(Spec[TaskCreateIn, TaskView]{
		Name:        "task_create",
		Description: "Create a to-do task. due is optional local time 'YYYY-MM-DD HH:MM'.",
		Schema: `{"type":"object","properties":{"title":{"type":"string"},"notes":{"type":"string"},
			"priority":{"type":"integer","enum":[1,2,3]},"due":{"type":"string"}},"required":["title"],"additionalProperties":false}`,
		Permission: Write,
		Timeout:    5 * time.Second,
		Retry:      RetryPolicy{Attempts: 2, Backoff: 200 * time.Millisecond},
		Confirm: func(_ TaskCreateIn, t TaskView) string {
			s := fmt.Sprintf("✅ Added task #%d: %s", t.ID, t.Title)
			if t.Due != "" {
				s += " (due " + t.Due + ")"
			}
			if t.Priority == 1 {
				s += " — high priority"
			}
			return s
		},
		Run: func(ctx context.Context, in TaskCreateIn) (TaskView, error) {
			nt := memory.NewTask{Title: in.Title, Notes: in.Notes, Priority: in.Priority}
			if in.Due != "" {
				due, err := ParseLocalTime(in.Due, d.Loc, d.now())
				if err != nil {
					return TaskView{}, err
				}
				nt.DueAt = &due
			}
			t, err := d.Store.CreateTask(ctx, nt)
			if err != nil {
				return TaskView{}, err
			}
			return taskView(t, d.Loc), nil
		},
	})
}

// --- task_list ---------------------------------------------------------------

type TaskListIn struct {
	Status string `json:"status,omitempty"` // pending | done | all
}

func (in *TaskListIn) Validate() error {
	switch in.Status {
	case "", "pending", "done", "all":
		return nil
	}
	return errors.New("status must be pending, done or all")
}

func TaskList(d Deps) Tool {
	return New(Spec[TaskListIn, []TaskView]{
		Name:        "task_list",
		Description: "List tasks (default: pending).",
		Schema:      `{"type":"object","properties":{"status":{"type":"string","enum":["pending","done","all"]}},"additionalProperties":false}`,
		Permission:  Read,
		Timeout:     5 * time.Second,
		Run: func(ctx context.Context, in TaskListIn) ([]TaskView, error) {
			status := in.Status
			switch status {
			case "", "pending":
				status = memory.TaskPending
			case "all":
				status = ""
			}
			ts, err := d.Store.ListTasks(ctx, status, 30)
			if err != nil {
				return nil, err
			}
			out := make([]TaskView, 0, len(ts))
			for _, t := range ts {
				out = append(out, taskView(t, d.Loc))
			}
			return out, nil
		},
	})
}

// --- task_complete / task_delete --------------------------------------------

type TaskIDIn struct {
	ID int64 `json:"id"`
}

func (in *TaskIDIn) Validate() error {
	if in.ID <= 0 {
		return errors.New("id must be a positive task id")
	}
	return nil
}

func TaskComplete(d Deps) Tool {
	return New(Spec[TaskIDIn, TaskView]{
		Name:        "task_complete",
		Description: "Mark a task as done by id.",
		Schema:      `{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"],"additionalProperties":false}`,
		Permission:  Write,
		Timeout:     5 * time.Second,
		Confirm: func(_ TaskIDIn, t TaskView) string {
			return fmt.Sprintf("🎉 Completed task #%d: %s", t.ID, t.Title)
		},
		Run: func(ctx context.Context, in TaskIDIn) (TaskView, error) {
			t, err := d.Store.SetTaskStatus(ctx, in.ID, memory.TaskDone)
			if errors.Is(err, memory.ErrNotFound) {
				return TaskView{}, fmt.Errorf("task %d not found", in.ID)
			}
			return taskView(t, d.Loc), err
		},
	})
}

// TaskDelete is irreversible, so it is Sensitive (always needs approval).
func TaskDelete(d Deps) Tool {
	return New(Spec[TaskIDIn, map[string]any]{
		Name:        "task_delete",
		Description: "Permanently delete a task by id (requires user approval).",
		Schema:      `{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"],"additionalProperties":false}`,
		Permission:  Sensitive,
		Timeout:     5 * time.Second,
		Summarize: func(in TaskIDIn) string {
			if t, err := d.Store.GetTask(context.Background(), in.ID); err == nil {
				return fmt.Sprintf("Permanently delete task #%d %q", t.ID, t.Title)
			}
			return fmt.Sprintf("Permanently delete task #%d", in.ID)
		},
		Run: func(ctx context.Context, in TaskIDIn) (map[string]any, error) {
			if err := d.Store.DeleteTask(ctx, in.ID); err != nil {
				return nil, fmt.Errorf("delete task %d: %w", in.ID, err)
			}
			return map[string]any{"deleted": in.ID}, nil
		},
	})
}

// --- plan_data ---------------------------------------------------------------

// PlanDataOut gathers what a daily plan needs.
type PlanDataOut struct {
	Now       string         `json:"now"`
	Pending   []TaskView     `json:"pending_tasks"`
	Overdue   []TaskView     `json:"overdue_tasks"`
	Reminders []ReminderView `json:"reminders_next_24h"`
}

type EmptyIn struct{}

func PlanData(d Deps) Tool {
	return New(Spec[EmptyIn, PlanDataOut]{
		Name:        "plan_data",
		Description: "Get pending/overdue tasks and the next 24h of reminders for planning the day.",
		Schema:      `{"type":"object","properties":{},"additionalProperties":false}`,
		Permission:  Read,
		Timeout:     5 * time.Second,
		Run: func(ctx context.Context, _ EmptyIn) (PlanDataOut, error) {
			return BuildPlanData(ctx, d)
		},
	})
}

// BuildPlanData is shared by the tool and the scheduled daily plan job.
func BuildPlanData(ctx context.Context, d Deps) (PlanDataOut, error) {
	now := d.now()
	out := PlanDataOut{Now: FormatLocal(now, d.Loc)}
	ts, err := d.Store.ListTasks(ctx, memory.TaskPending, 30)
	if err != nil {
		return out, err
	}
	for _, t := range ts {
		v := taskView(t, d.Loc)
		if t.DueAt != nil && t.DueAt.Before(now) {
			out.Overdue = append(out.Overdue, v)
		} else {
			out.Pending = append(out.Pending, v)
		}
	}
	rs, err := listReminders(ctx, d)
	if err != nil {
		return out, err
	}
	for _, r := range rs {
		if r.at.Before(now.Add(24 * time.Hour)) {
			out.Reminders = append(out.Reminders, r.ReminderView)
		}
	}
	return out, nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
