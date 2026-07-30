package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
)

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

// HeadCommit resolves HEAD in dir. Exported because the CLI needs it to default
// `deploy` to the current commit.
func HeadCommit(dir string) (string, error) {
	return git(dir, "rev-parse", "HEAD")
}

// ShortHash is the slot-naming form of a commit: eight characters, which is what
// makes slot directories readable on disk.
func ShortHash(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
