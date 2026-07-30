package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type agentStore struct {
	db *sql.DB
}

type conversationRow struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	SessionID    string `json:"session_id,omitempty"`
	User         string `json:"user,omitempty"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	CacheRead    int    `json:"cache_read"`
	CacheWrite   int    `json:"cache_write"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	Status       string `json:"status"`
	PID          int    `json:"pid,omitempty"`
}

type messageRow struct {
	ID             int64  `json:"id"`
	ConversationID string `json:"conversation_id"`
	Type           string `json:"type"`
	Content        string `json:"content"`
	CreatedAt      string `json:"created_at"`
}

// openAgentStore opens (creating if needed) the agent database.
//
// The pragmas travel in the DSN using modernc.org/sqlite's `_pragma=` syntax,
// not as PRAGMA statements. Two reasons, both learned the hard way:
//
//   - This driver silently ignores unknown DSN parameters. The previous
//     `?_journal_mode=WAL&_busy_timeout=5000` is mattn/go-sqlite3 syntax; here it
//     did nothing at all, leaving the database in rollback-journal mode with no
//     busy timeout. Under one streaming agent plus an open chat tab that produced
//     a steady stream of SQLITE_BUSY, and since every caller discarded the error,
//     agent output was dropped with no trace.
//   - database/sql pools connections, and busy_timeout is per-connection state.
//     A one-shot `PRAGMA busy_timeout` would apply only to whichever pooled
//     connection happened to serve it. `_pragma=` is applied by the driver to
//     every connection it opens, which is what we need.
//
// verifyPragmas below asserts they took effect, so this cannot regress into
// silence again.
func openAgentStore(path string) (*agentStore, error) {
	db, err := sql.Open("sqlite",
		path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}

	// WAL tolerates concurrent readers alongside one writer, but the write lock
	// is still exclusive; an unbounded pool just means more connections queueing
	// on busy_timeout. Four is comfortably above the real concurrency (one agent
	// writing, a couple of SSE readers) and keeps contention bounded.
	db.SetMaxOpenConns(4)

	if err := verifyPragmas(db); err != nil {
		db.Close()
		return nil, err
	}

	schema := `
	CREATE TABLE IF NOT EXISTS conversations (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL DEFAULT '',
		session_id TEXT NOT NULL DEFAULT '',
		user TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read INTEGER NOT NULL DEFAULT 0,
		cache_write INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id TEXT NOT NULL REFERENCES conversations(id),
		type TEXT NOT NULL,
		content TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, id);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema init: %w", err)
	}

	// Additive migrations. ALTER TABLE ADD COLUMN errors when the column already
	// exists, which is the idempotency check.
	for _, m := range []string{
		`ALTER TABLE conversations ADD COLUMN status TEXT NOT NULL DEFAULT 'idle'`,
		`ALTER TABLE conversations ADD COLUMN pid INTEGER NOT NULL DEFAULT 0`,
	} {
		db.Exec(m)
	}

	return &agentStore{db: db}, nil
}

// verifyPragmas fails startup if the DSN pragmas did not take effect. The bug
// this guards against is silent by nature: everything works until the store is
// under load, and then writes start failing one at a time.
func verifyPragmas(db *sql.DB) error {
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("reading journal_mode: %w", err)
	}
	if journal != "wal" {
		return fmt.Errorf("journal_mode is %q, expected \"wal\" — DSN pragmas were not applied", journal)
	}

	var busy int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		return fmt.Errorf("reading busy_timeout: %w", err)
	}
	if busy == 0 {
		return fmt.Errorf("busy_timeout is 0 — DSN pragmas were not applied")
	}
	return nil
}

func (s *agentStore) close() error { return s.db.Close() }

func (s *agentStore) createConversation(id, user string) (*conversationRow, error) {
	now := time.Now().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO conversations (id, user, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, user, now, now,
	)
	if err != nil {
		return nil, err
	}
	return &conversationRow{ID: id, User: user, CreatedAt: now, UpdatedAt: now, Status: "idle"}, nil
}

const conversationColumns = `id, title, session_id, user, input_tokens, output_tokens,
	cache_read, cache_write, created_at, updated_at, status, pid`

type scanner interface{ Scan(...any) error }

func scanConversation(sc scanner) (conversationRow, error) {
	var c conversationRow
	err := sc.Scan(&c.ID, &c.Title, &c.SessionID, &c.User,
		&c.InputTokens, &c.OutputTokens, &c.CacheRead, &c.CacheWrite,
		&c.CreatedAt, &c.UpdatedAt, &c.Status, &c.PID)
	return c, err
}

func (s *agentStore) getConversation(id string) (*conversationRow, error) {
	row := s.db.QueryRow(
		`SELECT `+conversationColumns+` FROM conversations WHERE id = ?`, id)
	c, err := scanConversation(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *agentStore) queryConversations(where string) ([]conversationRow, error) {
	rows, err := s.db.Query(`SELECT ` + conversationColumns + ` FROM conversations ` + where)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []conversationRow
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, rows.Err()
}

func (s *agentStore) listConversations() ([]conversationRow, error) {
	return s.queryConversations(`ORDER BY updated_at DESC`)
}

// queuedConversations returns conversations waiting for the agent slot, oldest
// first — the drain order is arrival order.
func (s *agentStore) queuedConversations() ([]conversationRow, error) {
	return s.queryConversations(`WHERE status = 'queued' ORDER BY updated_at ASC`)
}

// unfinishedConversations returns rows the database believes are queued or
// mid-run. After a daemon restart these are stale by definition, since the
// manager's in-memory map starts empty.
func (s *agentStore) unfinishedConversations() ([]conversationRow, error) {
	return s.queryConversations(`WHERE status IN ('running', 'queued')`)
}

func (s *agentStore) addMessage(conversationID, msgType, content string) (int64, error) {
	now := time.Now().Format(time.RFC3339)
	res, err := s.db.Exec(
		`INSERT INTO messages (conversation_id, type, content, created_at) VALUES (?, ?, ?, ?)`,
		conversationID, msgType, content, now,
	)
	if err != nil {
		return 0, err
	}
	if _, err := s.db.Exec(
		`UPDATE conversations SET updated_at = ? WHERE id = ?`, now, conversationID); err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *agentStore) getMessages(conversationID string, afterID int64) ([]messageRow, error) {
	rows, err := s.db.Query(
		`SELECT id, conversation_id, type, content, created_at
		 FROM messages WHERE conversation_id = ? AND id > ? ORDER BY id`,
		conversationID, afterID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []messageRow
	for rows.Next() {
		var m messageRow
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Type, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	return list, rows.Err()
}

func (s *agentStore) deleteConversation(id string) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE conversation_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM conversations WHERE id = ?`, id)
	return err
}

func (s *agentStore) updateSessionID(id, sessionID string) error {
	_, err := s.db.Exec(`UPDATE conversations SET session_id = ? WHERE id = ?`, sessionID, id)
	return err
}

// clearSessionID drops a resume target that no longer exists on disk, so the
// next turn starts a fresh session instead of failing opaquely.
func (s *agentStore) clearSessionID(id string) error {
	_, err := s.db.Exec(`UPDATE conversations SET session_id = '' WHERE id = ?`, id)
	return err
}

func (s *agentStore) updateTitle(id, title string) error {
	_, err := s.db.Exec(`UPDATE conversations SET title = ? WHERE id = ?`, title, id)
	return err
}

func (s *agentStore) addUsage(id string, input, output, cacheRead, cacheWrite int) error {
	_, err := s.db.Exec(
		`UPDATE conversations SET
			input_tokens = input_tokens + ?,
			output_tokens = output_tokens + ?,
			cache_read = cache_read + ?,
			cache_write = cache_write + ?
		 WHERE id = ?`,
		input, output, cacheRead, cacheWrite, id,
	)
	return err
}

func (s *agentStore) setConversationStatus(id, status string) error {
	_, err := s.db.Exec(
		`UPDATE conversations SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Format(time.RFC3339), id,
	)
	return err
}

// setConversationPID records the OS pid of the running agent so a restarted
// daemon can tell a genuinely-dead session from one whose process outlived it.
func (s *agentStore) setConversationPID(id string, pid int) error {
	_, err := s.db.Exec(`UPDATE conversations SET pid = ? WHERE id = ?`, pid, id)
	return err
}
