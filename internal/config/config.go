// Package config defines slot-machine's app contract: what the daemon needs to
// know about an app in order to run it, and nothing more.
//
// Defaults and validation live here rather than at the call sites, because the
// zero values are not harmless — a health timeout of 0 fails every deploy, and a
// drain timeout of 0 turns graceful shutdown into an immediate kill.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

type Config struct {
	SetupCommand    string `json:"setup_command"`
	StartCommand    string `json:"start_command"`
	Port            int    `json:"port"`
	InternalPort    int    `json:"internal_port"`
	HealthEndpoint  string `json:"health_endpoint"`
	HealthTimeoutMs int    `json:"health_timeout_ms"`
	DrainTimeoutMs  int    `json:"drain_timeout_ms"`
	EnvFile         string `json:"env_file"`
	APIPort         int    `json:"api_port"`

	AgentAuth           string   `json:"agent_auth"`            // "hmac" (default), "trusted", "none"
	AgentAllowedTools   []string `json:"agent_allowed_tools"`   // claude --allowed-tools
	AgentDeniedCommands []string `json:"agent_denied_commands"` // extra Bash prefixes to deny
	AgentModel          string   `json:"agent_model"`           // claude --model
	AgentTimeoutS       int      `json:"agent_timeout_s"`       // max seconds for one agent turn
	SharedDirs          []string `json:"shared_dirs"`           // dirs symlinked to a shared location
	ChatTitle           string   `json:"chat_title"`
	ChatAccent          string   `json:"chat_accent"`

	// Branch model. The agent commits to MachineBranch; humans work on
	// HumanBranch. See docs/design.md §4.
	MachineBranch string `json:"machine_branch"`
	HumanBranch   string `json:"human_branch"`

	// Pre-promotion gate. See docs/design.md §5.
	ProtectedPaths     []string `json:"protected_paths"`       // paths a deploy may not touch
	SecretPatterns     []string `json:"secret_patterns"`       // extra regexes, added to the built-ins
	MaxDiffLines       int      `json:"max_diff_lines"`        // 0 disables the check
	PreDeployCommand   string   `json:"pre_deploy_command"`    // run in staging; non-zero blocks the deploy
	PreDeployTimeoutMs int      `json:"pre_deploy_timeout_ms"` //

	// Migration policy. Purely observational: the orchestrator reads this
	// endpoint and makes a pass/fail decision, it never runs a migration.
	// See docs/migration-policy.md. Unset disables the check entirely.
	SchemaStatusEndpoint string `json:"schema_status_endpoint"`
}

// DefaultAllowedTools is the tool set an agent gets when the config is silent.
var DefaultAllowedTools = []string{"Bash", "Edit", "Read", "Write", "Glob", "Grep"}

// loadConfig reads, defaults and validates the config in one place.
//
// Defaults used to live only in `init`, so a hand-written or hand-edited config
// silently inherited Go zero values instead — and the zero values are not
// harmless. health_timeout_ms of 0 makes the health-check deadline expire before
// the first poll, so every deploy fails; drain_timeout_ms of 0 turns the
// graceful shutdown window into an immediate SIGKILL.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.HealthTimeoutMs == 0 {
		c.HealthTimeoutMs = 10000
	}
	if c.DrainTimeoutMs == 0 {
		c.DrainTimeoutMs = 5000
	}
	if c.APIPort == 0 {
		c.APIPort = 9100
	}
	if c.HealthEndpoint == "" {
		c.HealthEndpoint = "/healthz"
	}
	if c.AgentAuth == "" {
		c.AgentAuth = "hmac"
	}
	if len(c.AgentAllowedTools) == 0 {
		c.AgentAllowedTools = DefaultAllowedTools
	}
	if c.AgentTimeoutS == 0 {
		c.AgentTimeoutS = 1800 // 30 minutes
	}
	if c.ChatTitle == "" {
		c.ChatTitle = "slot-machine"
	}
	if c.MachineBranch == "" {
		c.MachineBranch = "machine"
	}
	if c.HumanBranch == "" {
		c.HumanBranch = "main"
	}
	if c.PreDeployTimeoutMs == 0 {
		c.PreDeployTimeoutMs = 120000
	}
	if c.InternalPort == 0 {
		c.InternalPort = c.Port
	}
}

func (c *Config) validate() error {
	var problems []string

	if c.StartCommand == "" {
		problems = append(problems, "start_command is required")
	}
	if c.Port == 0 {
		problems = append(problems, "port is required (the daemon reverse-proxies it to the live slot)")
	}
	if !strings.HasPrefix(c.HealthEndpoint, "/") {
		problems = append(problems, fmt.Sprintf("health_endpoint %q must start with /", c.HealthEndpoint))
	}
	switch c.AgentAuth {
	case "hmac", "trusted", "none":
	default:
		problems = append(problems, fmt.Sprintf("agent_auth %q must be hmac, trusted or none", c.AgentAuth))
	}
	if c.Port == c.APIPort {
		problems = append(problems, fmt.Sprintf("port and api_port are both %d; they must differ", c.Port))
	}
	if c.MachineBranch == c.HumanBranch {
		problems = append(problems, fmt.Sprintf(
			"machine_branch and human_branch are both %q; the agent needs its own branch", c.MachineBranch))
	}
	for _, p := range c.SecretPatterns {
		if _, err := regexp.Compile(p); err != nil {
			problems = append(problems, fmt.Sprintf("secret_patterns: %q is not a valid regexp: %v", p, err))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}
