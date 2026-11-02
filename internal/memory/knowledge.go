package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Knowledge sources. External (web) content is always tagged "research" so
// answers can distinguish it from what the user told us.
const (
	SourceUser         = "user"
	SourceResearch     = "research"
	SourceConversation = "conversation"
)

// KnowledgeEntry is a stored fact, note or research summary.
type KnowledgeEntry struct {
	ID        int64
	Title     string
	Body      string
	Source    string
	URL       string
	Tags      string
	CreatedAt Time
	UpdatedAt Time
}

// KnowledgeHit is a search result.
type KnowledgeHit struct {
	KnowledgeEntry
	Snippet string
}

// NewKnowledge is the input to AddKnowledge.
type NewKnowledge struct {
	Title, Body, Source, URL string
	Tags                     []string
}

// AddKnowledge stores an entry.
func (s *Store) AddKnowledge(ctx context.Context, in NewKnowledge) (KnowledgeEntry, error) {
	in.Title, in.Body = strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if in.Title == "" || in.Body == "" {
		return KnowledgeEntry{}, errors.New("knowledge: title and body are required")
	}
	switch in.Source {
	case SourceUser, SourceResearch, SourceConversation:
	default:
		return KnowledgeEntry{}, fmt.Errorf("knowledge: invalid source %q", in.Source)
	}
	now := s.now()
	tags := strings.Join(in.Tags, ",")
	res, err := s.db.ExecContext(ctx, `INSERT INTO knowledge(title, body, source, url, tags, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, in.Title, in.Body, in.Source, in.URL, tags, ts(now), ts(now))
	if err != nil {
		return KnowledgeEntry{}, fmt.Errorf("insert knowledge: %w", err)
	}
	id, _ := res.LastInsertId()
	return KnowledgeEntry{ID: id, Title: in.Title, Body: in.Body, Source: in.Source, URL: in.URL,
		Tags: tags, CreatedAt: Time(now), UpdatedAt: Time(now)}, nil
}

// GetKnowledge loads one entry.
func (s *Store) GetKnowledge(ctx context.Context, id int64) (KnowledgeEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, body, source, url, tags, created_at, updated_at
		FROM knowledge WHERE id = ?`, id)
	if err != nil {
		return KnowledgeEntry{}, err
	}
	defer rows.Close()
	list, err := scanKnowledge(rows)
	if err != nil {
		return KnowledgeEntry{}, err
	}
	if len(list) == 0 {
		return KnowledgeEntry{}, ErrNotFound
	}
	return list[0], nil
}

// ListKnowledge returns the newest entries.
func (s *Store) ListKnowledge(ctx context.Context, limit int) ([]KnowledgeEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, body, source, url, tags, created_at, updated_at
		FROM knowledge ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKnowledge(rows)
}

// SearchKnowledge runs an FTS5 search ranked by bm25 (title weighted 3x).
func (s *Store) SearchKnowledge(ctx context.Context, query string, limit int) ([]KnowledgeHit, error) {
	q := FTSQuery(query)
	if q == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT k.id, k.title, k.body, k.source, k.url, k.tags, k.created_at, k.updated_at,
		snippet(knowledge_fts, 1, '[', ']', '…', 16)
		FROM knowledge_fts JOIN knowledge k ON k.id = knowledge_fts.rowid
		WHERE knowledge_fts MATCH ? ORDER BY bm25(knowledge_fts, 3.0, 1.0, 2.0) LIMIT ?`, q, limit)
	if err != nil {
		return nil, fmt.Errorf("search knowledge: %w", err)
	}
	defer rows.Close()
	var out []KnowledgeHit
	for rows.Next() {
		var h KnowledgeHit
		var c, u string
		if err := rows.Scan(&h.ID, &h.Title, &h.Body, &h.Source, &h.URL, &h.Tags, &c, &u, &h.Snippet); err != nil {
			return nil, err
		}
		h.CreatedAt, h.UpdatedAt = Time(parseTS(c)), Time(parseTS(u))
		out = append(out, h)
	}
	return out, rows.Err()
}

// DeleteKnowledge removes an entry.
func (s *Store) DeleteKnowledge(ctx context.Context, id int64) error {
	return rowsAffected(s.db.ExecContext(ctx, `DELETE FROM knowledge WHERE id = ?`, id))
}

func scanKnowledge(rows *sql.Rows) ([]KnowledgeEntry, error) {
	var out []KnowledgeEntry
	for rows.Next() {
		var k KnowledgeEntry
		var c, u string
		if err := rows.Scan(&k.ID, &k.Title, &k.Body, &k.Source, &k.URL, &k.Tags, &c, &u); err != nil {
			return nil, err
		}
		k.CreatedAt, k.UpdatedAt = Time(parseTS(c)), Time(parseTS(u))
		out = append(out, k)
	}
	return out, rows.Err()
}
