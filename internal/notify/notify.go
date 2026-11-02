// Package notify fans out proactive messages (reminders, approvals,
// monitor alerts, daily plans) to the user's channels.
package notify

import (
	"context"
	"errors"

	"personal-assistant/internal/memory"
)

// Message is a proactive notification. Approval, when set, lets channels
// render approve/reject controls.
type Message struct {
	Text     string
	Approval *memory.Approval
}

// Notifier delivers notifications.
type Notifier interface {
	Notify(ctx context.Context, m Message) error
}

// Multi delivers to every notifier and joins errors.
type Multi []Notifier

// Notify implements Notifier.
func (m Multi) Notify(ctx context.Context, msg Message) error {
	var errs []error
	for _, n := range m {
		if n == nil {
			continue
		}
		if err := n.Notify(ctx, msg); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Store records notifications in the "system/notifications" conversation so
// the dashboard shows them even when Telegram is not configured.
type Store struct{ S *memory.Store }

// Notify implements Notifier.
func (s Store) Notify(ctx context.Context, m Message) error {
	c, err := s.S.GetOrCreateConversation(ctx, "system", "notifications")
	if err != nil {
		return err
	}
	_, err = s.S.AddMessage(ctx, c.ID, memory.RoleAssistant, "notify", m.Text)
	return err
}
