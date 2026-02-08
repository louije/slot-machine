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
}

type messageRow struct {
	ID             int64  `json:"id"`
	ConversationID string `json:"conversation_id"`
	Type           string `json:"type"`
	Content        string `json:"content"`
	CreatedAt      string `json:"created_at"`
}

func openAgentStore(path string) (*agentStore, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
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
	CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema init: %w", err)
	}

	return &agentStore{db: db}, nil
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
	return &conversationRow{ID: id, User: user, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *agentStore) getConversation(id string) (*conversationRow, error) {
	row := s.db.QueryRow(
		`SELECT id, title, session_id, user, input_tokens, output_tokens, cache_read, cache_write, created_at, updated_at
		 FROM conversations WHERE id = ?`, id,
	)
	var c conversationRow
	err := row.Scan(&c.ID, &c.Title, &c.SessionID, &c.User,
		&c.InputTokens, &c.OutputTokens, &c.CacheRead, &c.CacheWrite,
		&c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &c, err
}

func (s *agentStore) listConversations() ([]conversationRow, error) {
	rows, err := s.db.Query(
		`SELECT id, title, session_id, user, input_tokens, output_tokens, cache_read, cache_write, created_at, updated_at
		 FROM conversations ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []conversationRow
	for rows.Next() {
		var c conversationRow
		if err := rows.Scan(&c.ID, &c.Title, &c.SessionID, &c.User,
			&c.InputTokens, &c.OutputTokens, &c.CacheRead, &c.CacheWrite,
			&c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, nil
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
	s.db.Exec(`UPDATE conversations SET updated_at = ? WHERE id = ?`, now, conversationID)
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
	return list, nil
}

func (s *agentStore) updateSessionID(id, sessionID string) error {
	_, err := s.db.Exec(`UPDATE conversations SET session_id = ? WHERE id = ?`, sessionID, id)
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
