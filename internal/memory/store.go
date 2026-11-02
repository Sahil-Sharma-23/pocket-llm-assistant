// Package memory is the SQLite persistence layer: conversations, messages,
// preferences, tasks, knowledge (FTS5), agent runs, approvals, scheduled
// jobs and monitor results.
//
// The pure-Go modernc.org/sqlite driver is used so the binary stays
// CGO-free; it was verified on a real phone (android/arm64) with WAL and
// FTS5. Every timestamp is stored as RFC 3339 UTC text.
package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"personal-assistant/migrations"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Options configures Open.
type Options struct {
	BusyTimeout time.Duration
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
}

// Open opens (creating if needed) the database at path and applies pending
// migrations.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = 5 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", opts.BusyTimeout.Milliseconds()))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)") // durable across app crashes in WAL mode
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_txlock", "immediate") // writers take the lock up front: no upgrade deadlocks
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite allows one writer; a small pool is enough and bounds memory.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
		db.Close()
		return nil, fmt.Errorf("chmod db: %w", err)
	}
	s := &Store{db: db, now: opts.Now}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = ?`, name).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists > 0 {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, name, ts(s.now())); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// AppliedMigrations lists applied migration versions in order.
func (s *Store) AppliedMigrations(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Backup writes a consistent, compacted copy of the database to dest using
// VACUUM INTO (safe while the app is running).
func (s *Store) Backup(ctx context.Context, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("backup target %s already exists", dest)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	return os.Chmod(dest, 0o600)
}

// Stats are row counts for the dashboard.
type Stats struct {
	Conversations, Messages, Knowledge, PendingTasks, PendingApprovals, Jobs, Runs int
}

// Stats returns row counts.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM conversations),
		(SELECT count(*) FROM messages),
		(SELECT count(*) FROM knowledge),
		(SELECT count(*) FROM tasks WHERE status = 'pending'),
		(SELECT count(*) FROM approvals WHERE status = 'pending'),
		(SELECT count(*) FROM jobs WHERE enabled = 1),
		(SELECT count(*) FROM agent_runs)`).Scan(
		&st.Conversations, &st.Messages, &st.Knowledge, &st.PendingTasks,
		&st.PendingApprovals, &st.Jobs, &st.Runs)
	return st, err
}

// --- helpers ---------------------------------------------------------------

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func parseNullTS(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTS(ns.String)
	return &t
}

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

var ftsTerm = regexp.MustCompile(`[\p{L}\p{N}_]+`)

var stopwords = map[string]bool{
	"the": true, "an": true, "and": true, "or": true, "of": true, "to": true,
	"in": true, "on": true, "is": true, "it": true, "for": true, "me": true, "my": true,
	"what": true, "did": true, "do": true, "about": true, "we": true, "you": true,
}

// FTSQuery converts free text into a safe FTS5 MATCH expression: each word
// becomes a quoted prefix term, OR-ed together and ranked by bm25. User input
// can therefore never inject FTS5 syntax. Empty means "nothing searchable".
func FTSQuery(q string) string {
	words := ftsTerm.FindAllString(strings.ToLower(q), 12)
	var terms []string
	seen := map[string]bool{}
	for _, w := range words {
		if len([]rune(w)) < 2 || seen[w] || stopwords[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, `"`+w+`"*`)
	}
	return strings.Join(terms, " OR ")
}

func rowsAffected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
