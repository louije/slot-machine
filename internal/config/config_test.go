package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "slot-machine.json")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A config without the optional timeouts must not inherit Go zero values:
// health_timeout_ms of 0 fails every deploy, drain_timeout_ms of 0 turns a
// graceful shutdown into an immediate SIGKILL.

// A config without the optional timeouts must not inherit Go zero values:
// health_timeout_ms of 0 fails every deploy, drain_timeout_ms of 0 turns a
// graceful shutdown into an immediate SIGKILL.
func TestConfigAppliesDocumentedDefaults(t *testing.T) {
	t.Parallel()
	p := writeConfig(t, `{"start_command":"./run.sh","port":3000}`)

	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HealthTimeoutMs != 10000 {
		t.Fatalf("health_timeout_ms = %d, want 10000", cfg.HealthTimeoutMs)
	}
	if cfg.DrainTimeoutMs != 5000 {
		t.Fatalf("drain_timeout_ms = %d, want 5000", cfg.DrainTimeoutMs)
	}
	if cfg.APIPort != 9100 {
		t.Fatalf("api_port = %d, want 9100", cfg.APIPort)
	}
	if cfg.HealthEndpoint != "/healthz" {
		t.Fatalf("health_endpoint = %q", cfg.HealthEndpoint)
	}
	if cfg.MachineBranch != "machine" || cfg.HumanBranch != "main" {
		t.Fatalf("branches = %q/%q", cfg.MachineBranch, cfg.HumanBranch)
	}
	if cfg.AgentTimeoutS == 0 {
		t.Fatal("agent_timeout_s must have a default; an agent with no timeout can hang forever")
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, body, wantErr string
	}{
		{"missing start_command", `{"port":3000}`, "start_command"},
		{"missing port", `{"start_command":"./run.sh"}`, "port is required"},
		{"health endpoint without slash", `{"start_command":"x","port":3000,"health_endpoint":"healthz"}`, "must start with /"},
		{"unknown auth mode", `{"start_command":"x","port":3000,"agent_auth":"maybe"}`, "agent_auth"},
		{"port collides with api_port", `{"start_command":"x","port":9100}`, "must differ"},
		{"branches collide", `{"start_command":"x","port":3000,"machine_branch":"main"}`, "own branch"},
		{"bad secret pattern", `{"start_command":"x","port":3000,"secret_patterns":["(unclosed"]}`, "not a valid regexp"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Failure classification
// ---------------------------------------------------------------------------

func TestLoadEnvFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	content := `# comment
FOO=bar
BAZ=qux

# another comment
EMPTY=
NOEQ
`
	os.WriteFile(path, []byte(content), 0644)

	env, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}

	want := []string{"FOO=bar", "BAZ=qux", "EMPTY="}
	if len(env) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(env), len(want), env)
	}
	for i, w := range want {
		if env[i] != w {
			t.Errorf("env[%d] = %q, want %q", i, env[i], w)
		}
	}
}

func TestLoadEnvFileMissing(t *testing.T) {
	t.Parallel()
	_, err := LoadEnvFile("/nonexistent/.env")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
