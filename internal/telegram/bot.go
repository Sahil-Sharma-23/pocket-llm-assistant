package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"personal-assistant/internal/agent"
	"personal-assistant/internal/memory"
	"personal-assistant/internal/notify"
)

// Handler is the orchestrator surface the bot needs.
type Handler interface {
	Handle(ctx context.Context, in agent.Inbound) (agent.Reply, error)
	DecideApproval(ctx context.Context, id int64, approve bool, by string) (string, error)
}

// OffsetStore persists the update offset across restarts.
type OffsetStore interface {
	GetPreference(ctx context.Context, key string) (string, bool, error)
	SetPreference(ctx context.Context, key, value string) error
}

const offsetKey = "_telegram_offset"

// Options configures the bot.
type Options struct {
	Client      *Client
	Handler     Handler
	Offsets     OffsetStore
	AllowedIDs  []int64
	PollTimeout time.Duration
	Log         *slog.Logger
	// MaxInFlight bounds concurrently handled updates.
	MaxInFlight int
}

// Bot is the Telegram interface. Only allow-listed users are served.
type Bot struct {
	o        Options
	sem      chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	warned   map[int64]time.Time // rate-limits replies to unknown users
	username string
}

// New builds a bot.
func New(o Options) *Bot {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Bot{o: o, sem: make(chan struct{}, max(o.MaxInFlight, 1)), warned: map[int64]time.Time{}}
}

// SetHandler sets the orchestrator after construction (the bot is also a
// notifier the orchestrator depends on). Call before Run.
func (b *Bot) SetHandler(h Handler) { b.o.Handler = h }

// Commands shown in the Telegram menu.
var menu = [][2]string{
	{"help", "All commands"}, {"tasks", "Pending tasks"}, {"task", "Add a task"},
	{"remind", "Set a reminder"}, {"reminders", "Upcoming reminders"}, {"plan", "Today's plan"},
	{"note", "Save a note"}, {"recall", "Search notes and chats"}, {"search", "Research the web"},
	{"github", "Repository status"}, {"check", "Is a site up?"}, {"monitors", "Uptime summary"},
	{"approvals", "Pending approvals"}, {"status", "Assistant status"},
}

// Run long-polls until ctx is cancelled, then waits for in-flight handlers.
func (b *Bot) Run(ctx context.Context) error {
	me, err := b.o.Client.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram getMe (check the bot token): %w", err)
	}
	b.username = me.Username
	b.o.Log.Info("telegram bot connected", "bot", me.Username, "allowed_users", len(b.o.AllowedIDs))
	if len(b.o.AllowedIDs) == 0 {
		b.o.Log.Warn("telegram.allowed_user_ids is empty: the bot will answer nobody; message it once to learn your user id")
	}
	if err := b.o.Client.SetMyCommands(ctx, menu); err != nil {
		b.o.Log.Warn("set telegram commands", "err", err)
	}

	offset := b.loadOffset(ctx)
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		pollCtx, cancel := context.WithTimeout(ctx, b.o.PollTimeout+15*time.Second)
		ups, err := b.o.Client.GetUpdates(pollCtx, offset, b.o.PollTimeout)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			var api *APIError
			wait := backoff
			if errors.As(err, &api) && api.RetryAfter > 0 {
				wait = api.RetryAfter
			}
			b.o.Log.Warn("telegram poll failed", "err", err, "retry_in", wait.String())
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = 2 * time.Second
		for _, u := range ups {
			offset = u.UpdateID + 1
			// Persist before handling: a crash then skips the update instead
			// of replaying a possibly side-effecting request.
			b.saveOffset(ctx, offset)
			b.dispatch(ctx, u)
		}
	}
	b.wg.Wait()
	return nil
}

func (b *Bot) dispatch(ctx context.Context, u Update) {
	select {
	case b.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	b.wg.Add(1)
	go func() {
		defer func() { <-b.sem; b.wg.Done() }()
		defer func() {
			if p := recover(); p != nil {
				b.o.Log.Error("telegram handler panic", "panic", p)
			}
		}()
		switch {
		case u.Message != nil:
			b.handleMessage(ctx, u.Message)
		case u.CallbackQuery != nil:
			b.handleCallback(ctx, u.CallbackQuery)
		}
	}()
}

func (b *Bot) allowed(id int64) bool { return slices.Contains(b.o.AllowedIDs, id) }

func (b *Bot) handleMessage(ctx context.Context, m *Message) {
	if m.From == nil || m.Chat.Type != "private" {
		return // groups/channels are not supported
	}
	if !b.allowed(m.From.ID) {
		b.rejectUnknown(ctx, m)
		return
	}
	if m.Voice != nil {
		b.send(ctx, m.Chat.ID, "Voice messages aren't supported yet — please type your request.", nil)
		return
	}
	if strings.TrimSpace(m.Text) == "" {
		return
	}

	typingCtx, stopTyping := context.WithCancel(ctx)
	go b.keepTyping(typingCtx, m.Chat.ID)
	reply, err := b.o.Handler.Handle(ctx, agent.Inbound{
		Channel: "telegram", ExternalID: strconv.FormatInt(m.Chat.ID, 10), Text: m.Text,
	})
	stopTyping()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		b.o.Log.Error("handle telegram message", "err", err)
		b.send(ctx, m.Chat.ID, "Sorry, something went wrong: "+err.Error(), nil)
		return
	}
	text := reply.Text
	if len(reply.Approvals) > 0 {
		// Approval prompts get their own messages with buttons.
		text = ""
	}
	if text != "" {
		b.send(ctx, m.Chat.ID, text, nil)
	}
	for _, a := range reply.Approvals {
		b.sendApproval(ctx, m.Chat.ID, a)
	}
}

func (b *Bot) keepTyping(ctx context.Context, chatID int64) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		_ = b.o.Client.SendChatAction(ctx, chatID, "typing")
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *Bot) rejectUnknown(ctx context.Context, m *Message) {
	b.mu.Lock()
	last := b.warned[m.From.ID]
	if time.Since(last) < time.Hour {
		b.mu.Unlock()
		return
	}
	b.warned[m.From.ID] = time.Now()
	b.mu.Unlock()
	b.o.Log.Warn("telegram message from unauthorised user", "user_id", m.From.ID, "username", m.From.Username)
	b.send(ctx, m.Chat.ID, fmt.Sprintf("This is a private assistant. Your Telegram user id is %d. "+
		"If this is your bot, add that id to telegram.allowed_user_ids and restart.", m.From.ID), nil)
}

func (b *Bot) handleCallback(ctx context.Context, q *CallbackQuery) {
	if !b.allowed(q.From.ID) {
		_ = b.o.Client.AnswerCallbackQuery(ctx, q.ID, "Not authorised.")
		return
	}
	id, approve, ok := parseCallback(q.Data)
	if !ok {
		_ = b.o.Client.AnswerCallbackQuery(ctx, q.ID, "Unknown action.")
		return
	}
	msg, err := b.o.Handler.DecideApproval(ctx, id, approve, "telegram:"+strconv.FormatInt(q.From.ID, 10))
	if err != nil {
		msg = "Error: " + err.Error()
	}
	_ = b.o.Client.AnswerCallbackQuery(ctx, q.ID, clipRunes(msg, 190))
	if q.Message != nil {
		orig := q.Message.Text
		if err := b.o.Client.EditMessageText(ctx, q.Message.Chat.ID, q.Message.MessageID, orig+"\n\n→ "+msg); err != nil {
			b.send(ctx, q.Message.Chat.ID, msg, nil)
		}
	}
}

// parseCallback decodes "ap:<id>:y|n".
func parseCallback(data string) (int64, bool, bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "ap" || (parts[2] != "y" && parts[2] != "n") {
		return 0, false, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return 0, false, false
	}
	return id, parts[2] == "y", true
}

func approvalKeyboard(id int64) *InlineKeyboard {
	return &InlineKeyboard{InlineKeyboard: [][]InlineButton{{
		{Text: "✅ Approve", CallbackData: fmt.Sprintf("ap:%d:y", id)},
		{Text: "❌ Reject", CallbackData: fmt.Sprintf("ap:%d:n", id)},
	}}}
}

func (b *Bot) sendApproval(ctx context.Context, chatID int64, a memory.Approval) {
	b.send(ctx, chatID, agent.ApprovalPrompt(a), approvalKeyboard(a.ID))
}

// send splits long text at Telegram's 4096-character limit and honours
// flood-control retry_after once.
func (b *Bot) send(ctx context.Context, chatID int64, text string, kb *InlineKeyboard) {
	chunks := splitText(text, 4000)
	for i, c := range chunks {
		var k *InlineKeyboard
		if i == len(chunks)-1 {
			k = kb
		}
		_, err := b.o.Client.SendMessage(ctx, chatID, c, k)
		var api *APIError
		if errors.As(err, &api) && api.RetryAfter > 0 && api.RetryAfter < time.Minute {
			time.Sleep(api.RetryAfter)
			_, err = b.o.Client.SendMessage(ctx, chatID, c, k)
		}
		if err != nil {
			b.o.Log.Warn("telegram send failed", "err", err)
			return
		}
	}
}

// Notify implements notify.Notifier: it messages every allowed user.
func (b *Bot) Notify(ctx context.Context, m notify.Message) error {
	var errs []error
	for _, id := range b.o.AllowedIDs {
		if m.Approval != nil {
			b.sendApproval(ctx, id, *m.Approval)
			continue
		}
		if _, err := b.o.Client.SendMessage(ctx, id, clipRunes(m.Text, 4000), nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *Bot) loadOffset(ctx context.Context) int64 {
	v, ok, err := b.o.Offsets.GetPreference(ctx, offsetKey)
	if err != nil || !ok {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

func (b *Bot) saveOffset(ctx context.Context, off int64) {
	if err := b.o.Offsets.SetPreference(context.WithoutCancel(ctx), offsetKey, strconv.FormatInt(off, 10)); err != nil {
		b.o.Log.Warn("save telegram offset", "err", err)
	}
}

func splitText(s string, n int) []string {
	r := []rune(s)
	if len(r) <= n {
		return []string{s}
	}
	var out []string
	for len(r) > n {
		cut := n
		for i := n; i > n/2; i-- { // prefer a newline boundary
			if r[i] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		r = r[cut:]
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
