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

	// The security-relevant defaults. Each of these is the safe end of a choice
	// an operator can make, and each was at the unsafe end before: every
	// listener bound 0.0.0.0, and agent_auth defaulted to a mode that verified
	// nothing.
	if cfg.Listen != "127.0.0.1" {
		t.Errorf("listen = %q, want 127.0.0.1. slot-machine performs no authentication, "+
			"so the bind address is the security boundary and the default must be the "+
			"closed one.", cfg.Listen)
	}
	if cfg.AgentAuth != "header" {
		t.Errorf("agent_auth = %q, want header", cfg.AgentAuth)
	}
	if cfg.AgentAuthHeader != "X-Authenticated-User" {
		t.Errorf("agent_auth_header = %q, want X-Authenticated-User (what Caddy's "+
			"forward_auth and oauth2-proxy set)", cfg.AgentAuthHeader)
	}
	if cfg.AgentAccess != "allAuth" {
		t.Errorf("agent_access = %q, want allAuth. Bring your own door: an app that "+
			"does nothing keeps working, and delegating to the app is the opt-in for "+
			"when authenticated and allowed are different sets.", cfg.AgentAccess)
	}
	if cfg.AgentAccessEndpoint != "/_slot_machine/access" {
		t.Errorf("agent_access_endpoint = %q", cfg.AgentAccessEndpoint)
	}
}

// An operator upgrading past the removal of `hmac` has a config that names it,
// and a belief that it was doing something. The daemon refuses to start, which
// is the only moment that belief can be corrected — so the message has to carry
// the whole story, not just "invalid value".
func TestRemovedAuthModesExplainThemselves(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"hmac", "trusted"} {
		t.Run(mode, func(t *testing.T) {
			_, err := Load(writeConfig(t,
				`{"start_command":"x","port":3000,"agent_auth":"`+mode+`"}`))
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			msg := err.Error()
			for _, want := range []string{
				"removed",      // not "invalid": it used to work, and it never worked
				"/chat/config", // where the secret was served
				"\"header\"",   // what to write instead
				"\"none\"",     // and the local-development answer
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("the removal message does not mention %s:\n%s", want, msg)
				}
			}
		})
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
		{"unknown auth mode", `{"start_command":"x","port":3000,"agent_auth":"maybe"}`, "must be header or none"},
		{"unknown access mode", `{"start_command":"x","port":3000,"agent_access":"admins"}`, "must be app or allAuth"},
		{"access endpoint without slash", `{"start_command":"x","port":3000,"agent_access_endpoint":"access"}`, "must start with /"},
		{"header name with a colon", `{"start_command":"x","port":3000,"agent_auth_header":"X-User: v"}`, "not a valid header name"},
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
