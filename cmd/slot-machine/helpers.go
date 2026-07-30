package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "slot-machine: "+format+"\n", args...)
}

func loadEnvFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var env []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "=") {
			env = append(env, line)
		}
	}
	return env, scanner.Err()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// git runs a git command in dir and returns trimmed stdout. Stderr is folded
// into the error so failures explain themselves.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitOK reports whether a git command succeeds, for existence checks.
func gitOK(dir string, args ...string) bool {
	_, err := git(dir, args...)
	return err == nil
}

func gitHeadCommit(dir string) (string, error) {
	return git(dir, "rev-parse", "HEAD")
}

func shortHash(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
