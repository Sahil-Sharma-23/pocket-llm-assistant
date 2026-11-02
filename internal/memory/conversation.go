package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Conversation is a chat thread on one channel (a Telegram chat, the web UI).
type Conversation struct {
	ID         int64
	Channel    string
	ExternalID string
	Title      string
	CreatedAt  Time
	UpdatedAt  Time
	Messages   int // populated by ListConversations
}

// Message is one turn in a conversation.
type Message struct {
	ID             int64
	ConversationID int64
	Role           string
	Agent          string
	Content        string
	CreatedAt      Time
}

// MessageHit is a full-text search result over past messages.
type MessageHit struct {
	Message
	Channel string
	Snippet string
}

// Message roles.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// GetOrCreateConversation returns the conversation for (channel, externalID),
// creating it on first use.
func (s *Store) GetOrCreateConversation(ctx context.Context, channel, externalID string) (Conversation, error) {
	now := ts(s.now())
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversations(channel, external_id, created_at, updated_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(channel, external_id) DO NOTHING`, channel, externalID, now, now)
	if err != nil {
		return Conversation{}, fmt.Errorf("upsert conversation: %w", err)
	}
	var c Conversation
	var created, updated string
	err = s.db.QueryRowContext(ctx, `SELECT id, channel, external_id, title, created_at, updated_at
		FROM conversations WHERE channel = ? AND external_id = ?`, channel, externalID).
		Scan(&c.ID, &c.Channel, &c.ExternalID, &c.Title, &created, &updated)
	if err != nil {
		return Conversation{}, err
	}
	c.CreatedAt, c.UpdatedAt = Time(parseTS(created)), Time(parseTS(updated))
	return c, nil
}

// GetConversation loads a conversation by id.
func (s *Store) GetConversation(ctx context.Context, id int64) (Conversation, error) {
	var c Conversation
	var created, updated string
	err := s.db.QueryRowContext(ctx, `SELECT id, channel, external_id, title, created_at, updated_at
		FROM conversations WHERE id = ?`, id).
		Scan(&c.ID, &c.Channel, &c.ExternalID, &c.Title, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	c.CreatedAt, c.UpdatedAt = Time(parseTS(created)), Time(parseTS(updated))
	return c, err
}

// ListConversations returns the most recently active conversations.
func (s *Store) ListConversations(ctx context.Context, limit int) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.channel, c.external_id, c.title, c.created_at, c.updated_at,
		(SELECT count(*) FROM messages m WHERE m.conversation_id = c.id)
		FROM conversations c ORDER BY c.updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		var c Conversation
		var created, updated string
		if err := rows.Scan(&c.ID, &c.Channel, &c.ExternalID, &c.Title, &created, &updated, &c.Messages); err != nil {
			return nil, err
		}
		c.CreatedAt, c.UpdatedAt = Time(parseTS(created)), Time(parseTS(updated))
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddMessage appends a message and bumps the conversation's activity time.
func (s *Store) AddMessage(ctx context.Context, conversationID int64, role, agent, content string) (Message, error) {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO messages(conversation_id, role, agent, content, created_at)
		VALUES (?, ?, ?, ?, ?)`, conversationID, role, agent, content, ts(now))
	if err != nil {
		return Message{}, fmt.Errorf("insert message: %w", err)
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET updated_at = ?,
		title = CASE WHEN title = '' AND ? = 'user' THEN substr(?, 1, 60) ELSE title END
		WHERE id = ?`, ts(now), role, content, conversationID); err != nil {
		return Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, err
	}
	return Message{ID: id, ConversationID: conversationID, Role: role, Agent: agent, Content: content, CreatedAt: Time(now)}, nil
}

// RecentMessages returns the last n messages in chronological order.
func (s *Store) RecentMessages(ctx context.Context, conversationID int64, n int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, conversation_id, role, agent, content, created_at FROM (
		SELECT * FROM messages WHERE conversation_id = ? ORDER BY id DESC LIMIT ?) ORDER BY id`, conversationID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// SearchMessages runs a full-text search over all past messages.
func (s *Store) SearchMessages(ctx context.Context, query string, limit int) ([]MessageHit, error) {
	q := FTSQuery(query)
	if q == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.conversation_id, m.role, m.agent, m.content, m.created_at,
		c.channel, snippet(messages_fts, 0, '[', ']', '…', 12)
		FROM messages_fts JOIN messages m ON m.id = messages_fts.rowid
		JOIN conversations c ON c.id = m.conversation_id
		WHERE messages_fts MATCH ? ORDER BY bm25(messages_fts) LIMIT ?`, q, limit)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()
	var out []MessageHit
	for rows.Next() {
		var h MessageHit
		var created string
		if err := rows.Scan(&h.ID, &h.ConversationID, &h.Role, &h.Agent, &h.Content, &created, &h.Channel, &h.Snippet); err != nil {
			return nil, err
		}
		h.CreatedAt = Time(parseTS(created))
		out = append(out, h)
	}
	return out, rows.Err()
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var m Message
		var created string
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Agent, &m.Content, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = Time(parseTS(created))
		out = append(out, m)
	}
	return out, rows.Err()
}
