package store

import (
	"path/filepath"
	"sync"
	"testing"
)

// The regression this guards: the DSN previously used another driver's
// parameter syntax, which modernc.org/sqlite silently ignored, leaving the
// store in rollback-journal mode with no busy timeout.
func TestStorePragmasApplied(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var journal string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}

	var busy int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if busy == 0 {
		t.Fatal("busy_timeout is 0; concurrent writes will fail with SQLITE_BUSY")
	}
}

// Concurrent readers and writers must not lose events. Before the pragma fix
// this produced well over a thousand SQLITE_BUSY errors, every one of them
// discarded by the caller.

// Concurrent readers and writers must not lose events. Before the pragma fix
// this produced well over a thousand SQLITE_BUSY errors, every one of them
// discarded by the caller.
func TestStoreSurvivesConcurrentAccess(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.CreateConversation("c1", "u"); err != nil {
		t.Fatal(err)
	}

	const writers, readers, iterations = 3, 3, 120

	var wg sync.WaitGroup
	errCh := make(chan error, (writers+readers)*iterations*2)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if _, err := s.AddMessage("c1", "assistant", `{"content":"hello"}`); err != nil {
					errCh <- err
				}
			}
		}()
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if _, err := s.GetMessages("c1", 0); err != nil {
					errCh <- err
				}
				if _, err := s.GetConversation("c1"); err != nil {
					errCh <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)

	n := 0
	var first error
	for err := range errCh {
		if first == nil {
			first = err
		}
		n++
	}
	if n > 0 {
		t.Fatalf("%d store errors under concurrent access; first: %v", n, first)
	}

	msgs, err := s.GetMessages("c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != writers*iterations {
		t.Fatalf("stored %d messages, want %d — writes were lost", len(msgs), writers*iterations)
	}
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func TestStoreStatusMigration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s1, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	conv, _ := s2.CreateConversation("c1", "user1")
	if conv.Status != "idle" {
		t.Fatalf("expected status 'idle', got %q", conv.Status)
	}
}

func TestSetConversationStatus(t *testing.T) {
	t.Parallel()
	s, _ := Open(filepath.Join(t.TempDir(), "test.db"))
	defer s.Close()

	s.CreateConversation("c1", "user1")

	if err := s.SetConversationStatus("c1", "running"); err != nil {
		t.Fatal(err)
	}
	conv, _ := s.GetConversation("c1")
	if conv.Status != "running" {
		t.Fatalf("expected 'running', got %q", conv.Status)
	}
}
