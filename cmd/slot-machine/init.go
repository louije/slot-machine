package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"slot-machine/internal/config"
)

func cmdInit() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	cfg := config.Config{
		Port:            3000,
		InternalPort:    3000,
		HealthEndpoint:  "/healthz",
		HealthTimeoutMs: 10000,
		DrainTimeoutMs:  5000,
		APIPort:         9100,

		// Written out even though they are the defaults, because these are the
		// fields that decide who can reach an agent that commits and deploys.
		// A reader of this file should not have to know that the absent value
		// and the safe value coincide — and the previous scaffold emitted
		// `"agent_auth": ""`, which reads like "no authentication" and means
		// the opposite.
		Listen:          "127.0.0.1",
		AgentAuth:       "header",
		AgentAuthHeader: "X-Authenticated-User",
		AgentAccess:     "allAuth",
	}

	switch {
	case fileExists(filepath.Join(cwd, "bun.lock")):
		cfg.SetupCommand = "bun install --frozen-lockfile"
		cfg.StartCommand = readStartScript(cwd, "bun")
	case fileExists(filepath.Join(cwd, "package-lock.json")):
		cfg.SetupCommand = "npm ci"
		cfg.StartCommand = readStartScript(cwd, "node")
	case fileExists(filepath.Join(cwd, "uv.lock")):
		cfg.SetupCommand = "uv sync --frozen"
		cfg.StartCommand = "uv run python app.py"
	case fileExists(filepath.Join(cwd, "Gemfile.lock")):
		cfg.SetupCommand = "bundle install"
		cfg.StartCommand = "bundle exec ruby app.rb"
	}

	if fileExists(filepath.Join(cwd, ".env")) {
		cfg.EnvFile = ".env"
	}

	data, err := scaffold(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building config: %v\n", err)
		os.Exit(1)
	}
	cfgPath := filepath.Join(cwd, "slot-machine.json")
	if err := os.WriteFile(cfgPath, append(data, '\n'), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", cfgPath, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", cfgPath)

	gitignorePath := filepath.Join(cwd, ".gitignore")
	if !gitignoreContains(gitignorePath, ".slot-machine") {
		f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			if info, _ := f.Stat(); info.Size() > 0 {
				buf := make([]byte, 1)
				if fRead, err := os.Open(gitignorePath); err == nil {
					fRead.Seek(-1, io.SeekEnd)
					fRead.Read(buf)
					fRead.Close()
					if buf[0] != '\n' {
						f.WriteString("\n")
					}
				}
			}
			f.WriteString(".slot-machine\n")
			f.Close()
			fmt.Println("added .slot-machine to .gitignore")
		}
	}
}

// scaffold renders a config with the fields this command actually set, and
// nothing else.
//
// Marshalling the struct directly emitted all twenty-nine fields, most of them
// empty — `"max_diff_lines": 0`, `"secret_patterns": null`. That is noise around
// the four lines a new operator has to read, and worse than noise for the auth
// fields: `"agent_auth": ""` looks like a decision to run without
// authentication, when in fact the empty value defaults to requiring it.
//
// Round-tripping through the encoder rather than hand-writing JSON keeps the
// output valid whatever ends up in a command or path, and keeps field names tied
// to the struct tags instead of duplicating them here.
func scaffold(cfg config.Config) ([]byte, error) {
	full, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(full, &all); err != nil {
		return nil, err
	}

	// Field order is deliberate: what the app is, then how it is reached, then
	// who may reach the agent. Anything absent here was not set above.
	order := []string{
		"setup_command", "start_command",
		"port", "internal_port", "health_endpoint",
		"health_timeout_ms", "drain_timeout_ms",
		"env_file", "api_port", "listen",
		"agent_auth", "agent_auth_header", "agent_access",
	}

	var b strings.Builder
	b.WriteString("{\n")
	first := true
	for _, name := range order {
		raw, ok := all[name]
		if !ok || isEmptyJSON(raw) {
			continue
		}
		if !first {
			b.WriteString(",\n")
		}
		first = false
		fmt.Fprintf(&b, "  %q: %s", name, raw)
	}
	b.WriteString("\n}\n")
	return []byte(b.String()), nil
}

func isEmptyJSON(raw json.RawMessage) bool {
	switch string(raw) {
	case `""`, "0", "null", "[]", "{}", "false":
		return true
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readStartScript(dir, runtime string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return runtime + " index.js"
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
		Main    string            `json:"main"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return runtime + " index.js"
	}
	if s, ok := pkg.Scripts["start"]; ok {
		return s
	}
	if pkg.Main != "" {
		return runtime + " " + pkg.Main
	}
	return runtime + " index.js"
}

func gitignoreContains(path, entry string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == entry {
			return true
		}
	}
	return false
}
