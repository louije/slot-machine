package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitignoreContains(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")

	// Missing file.
	if gitignoreContains(path, ".slot-machine") {
		t.Fatal("expected false for missing file")
	}

	// File without entry.
	os.WriteFile(path, []byte("node_modules\n.env\n"), 0644)
	if gitignoreContains(path, ".slot-machine") {
		t.Fatal("expected false when entry absent")
	}

	// File with entry.
	os.WriteFile(path, []byte("node_modules\n.slot-machine\n.env\n"), 0644)
	if !gitignoreContains(path, ".slot-machine") {
		t.Fatal("expected true when entry present")
	}

	// Entry with surrounding whitespace.
	os.WriteFile(path, []byte("  .slot-machine  \n"), 0644)
	if !gitignoreContains(path, ".slot-machine") {
		t.Fatal("expected true with surrounding whitespace")
	}
}

func TestFileExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if fileExists(filepath.Join(dir, "nope")) {
		t.Fatal("expected false for nonexistent file")
	}
	path := filepath.Join(dir, "yes")
	os.WriteFile(path, []byte(""), 0644)
	if !fileExists(path) {
		t.Fatal("expected true for existing file")
	}
}

func TestReadStartScript(t *testing.T) {
	t.Parallel()

	t.Run("with start script", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "package.json"),
			[]byte(`{"scripts":{"start":"bun server/index.ts"}}`), 0644)
		got := readStartScript(dir, "bun")
		if got != "bun server/index.ts" {
			t.Fatalf("got %q, want bun server/index.ts", got)
		}
	})

	t.Run("with main field", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "package.json"),
			[]byte(`{"main":"server.js"}`), 0644)
		got := readStartScript(dir, "node")
		if got != "node server.js" {
			t.Fatalf("got %q, want node server.js", got)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{}`), 0644)
		got := readStartScript(dir, "node")
		if got != "node index.js" {
			t.Fatalf("got %q, want node index.js", got)
		}
	})

	t.Run("no package.json", func(t *testing.T) {
		dir := t.TempDir()
		got := readStartScript(dir, "bun")
		if got != "bun index.js" {
			t.Fatalf("got %q, want bun index.js", got)
		}
	})
}

func TestParseChecksum(t *testing.T) {
	t.Parallel()

	body := "" +
		"aaaa1111  slot-machine-linux-amd64\n" +
		"BBBB2222 *slot-machine-darwin-arm64\n" +
		"garbage line\n"

	t.Run("plain entry", func(t *testing.T) {
		got, err := parseChecksum(body, "slot-machine-linux-amd64")
		if err != nil {
			t.Fatal(err)
		}
		if got != "aaaa1111" {
			t.Fatalf("got %q", got)
		}
	})

	// sha256sum marks binary-mode entries with a leading "*"; the name still
	// matches.
	t.Run("binary-mode entry", func(t *testing.T) {
		got, err := parseChecksum(body, "slot-machine-darwin-arm64")
		if err != nil {
			t.Fatal(err)
		}
		if got != "bbbb2222" {
			t.Fatalf("got %q, want the hash lowercased", got)
		}
	})

	t.Run("missing entry is an error, not an empty hash", func(t *testing.T) {
		if _, err := parseChecksum(body, "slot-machine-windows-amd64"); err == nil {
			t.Fatal("expected an error for a name with no entry")
		}
	})
}

// ---------------------------------------------------------------------------
// System prompt bounds
// ---------------------------------------------------------------------------

// The prompt is one command-line argument, and Linux caps a single argument at
// 128 KiB. Exceeding it is E2BIG — no agent at all — so the instruction file is
// bounded rather than trusted.
