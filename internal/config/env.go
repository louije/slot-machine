package config

import (
	"bufio"
	"os"
	"strings"
)

// LoadEnvFile reads a dotenv-style file into a KEY=VALUE slice suitable for
// exec.Cmd.Env. Blank lines and comments are skipped; no expansion is performed,
// because the app's own runtime is what interprets these values.
func LoadEnvFile(path string) ([]string, error) {
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
