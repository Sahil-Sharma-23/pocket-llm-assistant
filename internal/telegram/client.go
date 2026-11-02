// Package telegram implements the Telegram Bot API interface using long
// polling (no webhook, so nothing on the phone is exposed to the internet).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a minimal Bot API client.
type Client struct {
	base  string // https://api.telegram.org
	token string
	http  *http.Client
}

// NewClient builds a client. The token is kept private and redacted from
// every error message.
func NewClient(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, http: hc}
}

// Types (subset of the Bot API).
type (
	User struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
	}
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	}
	Message struct {
		MessageID int64     `json:"message_id"`
		From      *User     `json:"from"`
		Chat      Chat      `json:"chat"`
		Text      string    `json:"text"`
		Voice     *struct{} `json:"voice"`
	}
	CallbackQuery struct {
		ID      string   `json:"id"`
		From    User     `json:"from"`
		Message *Message `json:"message"`
		Data    string   `json:"data"`
	}
	Update struct {
		UpdateID      int64          `json:"update_id"`
		Message       *Message       `json:"message"`
		CallbackQuery *CallbackQuery `json:"callback_query"`
	}
	InlineButton struct {
		Text         string `json:"text"`
		CallbackData string `json:"callback_data"`
	}
	InlineKeyboard struct {
		InlineKeyboard [][]InlineButton `json:"inline_keyboard"`
	}
)

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// APIError is a Bot API error.
type APIError struct {
	Code        int
	Description string
	RetryAfter  time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram api error %d: %s", e.Code, e.Description)
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return c.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return c.redact(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return c.redact(err)
	}
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return fmt.Errorf("telegram %s: http %d: invalid response", method, resp.StatusCode)
	}
	if !ar.OK {
		e := &APIError{Code: ar.ErrorCode, Description: ar.Description}
		if ar.Parameters != nil {
			e.RetryAfter = time.Duration(ar.Parameters.RetryAfter) * time.Second
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(ar.Result, out)
	}
	return nil
}

// redact removes the bot token from errors (url.Error embeds the full URL).
func (c *Client) redact(err error) error {
	if err == nil || c.token == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), c.token, "<redacted>")
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", msg, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", msg, context.DeadlineExceeded)
	}
	return errors.New(msg)
}

// GetMe validates the token.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	return u, c.call(ctx, "getMe", map[string]any{}, &u)
}

// GetUpdates long-polls for updates.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": int(timeout.Seconds()),
		"allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

// SendMessage sends plain text (no parse mode, so no escaping pitfalls).
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, kb *InlineKeyboard) (Message, error) {
	p := map[string]any{"chat_id": chatID, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}
	if kb != nil {
		p["reply_markup"] = kb
	}
	var m Message
	return m, c.call(ctx, "sendMessage", p, &m)
}

// EditMessageText replaces a message's text and removes its keyboard.
func (c *Client) EditMessageText(ctx context.Context, chatID, msgID int64, text string) error {
	return c.call(ctx, "editMessageText", map[string]any{"chat_id": chatID, "message_id": msgID, "text": text}, nil)
}

// AnswerCallbackQuery acknowledges a button press.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

// SendChatAction shows "typing…".
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	return c.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action}, nil)
}

// SetMyCommands registers the command menu shown in Telegram clients.
func (c *Client) SetMyCommands(ctx context.Context, cmds [][2]string) error {
	type bc struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	list := make([]bc, 0, len(cmds))
	for _, c := range cmds {
		list = append(list, bc{c[0], c[1]})
	}
	return c.call(ctx, "setMyCommands", map[string]any{"commands": list}, nil)
}
