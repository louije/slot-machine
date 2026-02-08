package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func cmdInit() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	cfg := config{
		Port:            3000,
		InternalPort:    3000,
		HealthEndpoint:  "/healthz",
		HealthTimeoutMs: 10000,
		DrainTimeoutMs:  5000,
		APIPort:         9100,
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

	data, _ := json.MarshalIndent(cfg, "", "  ")
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
