package main

type config struct {
	SetupCommand        string `json:"setup_command"`
	StartCommand        string `json:"start_command"`
	Port                int    `json:"port"`
	InternalPort        int    `json:"internal_port"`
	HealthEndpoint      string `json:"health_endpoint"`
	HealthTimeoutMs     int    `json:"health_timeout_ms"`
	DrainTimeoutMs      int    `json:"drain_timeout_ms"`
	EnvFile             string `json:"env_file"`
	APIPort             int    `json:"api_port"`
	AgentAuth           string   `json:"agent_auth"`            // "hmac" (default), "trusted", "none"
	AgentAllowedTools   []string `json:"agent_allowed_tools"`   // claude --allowed-tools (default: standard set)
	SharedDirs          []string `json:"shared_dirs"`           // dirs symlinked to shared persistent location
	ChatTitle           string   `json:"chat_title"`           // header title (default: "slot-machine")
	ChatAccent          string   `json:"chat_accent"`          // CSS accent color (default: "#2563eb")
}
