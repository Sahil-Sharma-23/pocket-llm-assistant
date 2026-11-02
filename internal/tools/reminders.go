package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"personal-assistant/internal/memory"
)

// JobKindReminder is the scheduler job kind for reminders.
const JobKindReminder = "reminder"

// ReminderPayload is stored in jobs.payload.
type ReminderPayload struct {
	Text string `json:"text"`
}

// ReminderView is the model-facing reminder shape.
type ReminderView struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
	At   string `json:"at"`
}

type reminderAt struct {
	ReminderView
	at time.Time
}

type ReminderCreateIn struct {
	Text      string `json:"text"`
	At        string `json:"at,omitempty"`         // local "YYYY-MM-DD HH:MM" or "HH:MM"
	InMinutes int    `json:"in_minutes,omitempty"` // alternative to At
}

func (in *ReminderCreateIn) Validate() error {
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" || len(in.Text) > 500 {
		return errors.New("text must be 1-500 characters")
	}
	if (in.At == "") == (in.InMinutes == 0) {
		return errors.New("give exactly one of at or in_minutes")
	}
	if in.InMinutes < 0 || in.InMinutes > 60*24*366 {
		return errors.New("in_minutes out of range")
	}
	return nil
}

func ReminderCreate(d Deps) Tool {
	return New(Spec[ReminderCreateIn, ReminderView]{
		Name:        "reminder_create",
		Description: "Schedule a reminder notification. Use at='YYYY-MM-DD HH:MM' (local) or in_minutes.",
		Schema: `{"type":"object","properties":{"text":{"type":"string"},"at":{"type":"string"},
			"in_minutes":{"type":"integer"}},"required":["text"],"additionalProperties":false}`,
		Permission: Write,
		Timeout:    5 * time.Second,
		Confirm: func(_ ReminderCreateIn, r ReminderView) string {
			return fmt.Sprintf("⏰ Reminder #%d set for %s: %s", r.ID, r.At, r.Text)
		},
		Run: func(ctx context.Context, in ReminderCreateIn) (ReminderView, error) {
			return CreateReminder(ctx, d, in)
		},
	})
}

// CreateReminder is shared with the /remind command.
func CreateReminder(ctx context.Context, d Deps, in ReminderCreateIn) (ReminderView, error) {
	if err := in.Validate(); err != nil {
		return ReminderView{}, err
	}
	now := d.now()
	at := now.Add(time.Duration(in.InMinutes) * time.Minute)
	if in.At != "" {
		var err error
		if at, err = ParseLocalTime(in.At, d.Loc, now); err != nil {
			return ReminderView{}, err
		}
	}
	if !at.After(now) {
		return ReminderView{}, fmt.Errorf("reminder time %s is in the past", FormatLocal(at, d.Loc))
	}
	payload, _ := json.Marshal(ReminderPayload{Text: in.Text})
	j, err := d.Store.CreateJob(ctx, memory.JobSpec{
		Kind: JobKindReminder, Name: reminderName(now),
		Payload: string(payload), Schedule: "once", NextRunAt: at,
	})
	if err != nil {
		return ReminderView{}, err
	}
	d.wake()
	return ReminderView{ID: j.ID, Text: in.Text, At: FormatLocal(at, d.Loc)}, nil
}

func ReminderList(d Deps) Tool {
	return New(Spec[EmptyIn, []ReminderView]{
		Name:        "reminder_list",
		Description: "List upcoming reminders.",
		Schema:      `{"type":"object","properties":{},"additionalProperties":false}`,
		Permission:  Read,
		Timeout:     5 * time.Second,
		Run: func(ctx context.Context, _ EmptyIn) ([]ReminderView, error) {
			rs, err := listReminders(ctx, d)
			out := make([]ReminderView, 0, len(rs))
			for _, r := range rs {
				out = append(out, r.ReminderView)
			}
			return out, err
		},
	})
}

func listReminders(ctx context.Context, d Deps) ([]reminderAt, error) {
	jobs, err := d.Store.ListJobs(ctx, JobKindReminder, 50)
	if err != nil {
		return nil, err
	}
	var out []reminderAt
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		var p ReminderPayload
		_ = json.Unmarshal([]byte(j.Payload), &p)
		out = append(out, reminderAt{ReminderView{ID: j.ID, Text: p.Text, At: FormatLocal(j.NextRunAt, d.Loc)}, j.NextRunAt})
	}
	return out, nil
}

type ReminderIDIn struct {
	ID int64 `json:"id"`
}

func (in *ReminderIDIn) Validate() error {
	if in.ID <= 0 {
		return errors.New("id must be a positive reminder id")
	}
	return nil
}

// ReminderCancel removes a scheduled notification; consequential, so it
// goes through approval.
func ReminderCancel(d Deps) Tool {
	return New(Spec[ReminderIDIn, map[string]any]{
		Name:          "reminder_cancel",
		Description:   "Cancel a scheduled reminder by id (requires approval).",
		Schema:        `{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"],"additionalProperties":false}`,
		Permission:    Write,
		Timeout:       5 * time.Second,
		Consequential: func(ReminderIDIn) bool { return true },
		Summarize: func(in ReminderIDIn) string {
			if j, err := d.Store.GetJob(context.Background(), in.ID); err == nil && j.Kind == JobKindReminder {
				var p ReminderPayload
				_ = json.Unmarshal([]byte(j.Payload), &p)
				return fmt.Sprintf("Cancel reminder #%d %q due %s", j.ID, p.Text, FormatLocal(j.NextRunAt, d.Loc))
			}
			return fmt.Sprintf("Cancel reminder #%d", in.ID)
		},
		Run: func(ctx context.Context, in ReminderIDIn) (map[string]any, error) {
			j, err := d.Store.GetJob(ctx, in.ID)
			if err != nil || j.Kind != JobKindReminder {
				return nil, fmt.Errorf("reminder %d not found", in.ID)
			}
			if err := d.Store.DeleteJob(ctx, in.ID); err != nil {
				return nil, err
			}
			d.wake()
			return map[string]any{"cancelled": in.ID}, nil
		},
	})
}

// reminderName is unique even for reminders created in the same instant.
func reminderName(now time.Time) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("r-%d-%s", now.Unix(), hex.EncodeToString(b))
}
