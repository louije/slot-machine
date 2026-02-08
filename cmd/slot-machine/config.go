package main

type config struct {
	SetupCommand    string `json:"setup_command"`
	StartCommand    string `json:"start_command"`
	Port            int    `json:"port"`
	InternalPort    int    `json:"internal_port"`
	HealthEndpoint  string `json:"health_endpoint"`
	HealthTimeoutMs int    `json:"health_timeout_ms"`
	DrainTimeoutMs  int    `json:"drain_timeout_ms"`
	EnvFile         string `json:"env_file"`
	APIPort         int    `json:"api_port"`
}
