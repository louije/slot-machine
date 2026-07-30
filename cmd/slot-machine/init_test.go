package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"slot-machine/internal/config"
)

// The scaffold is the first and often only description of this software an
// operator reads. Two properties matter, and neither is about formatting.
func TestScaffoldStatesTheSecurityChoicesOutLoud(t *testing.T) {
	t.Parallel()

	data, err := scaffold(config.Config{
		Port:            3000,
		InternalPort:    3000,
		HealthEndpoint:  "/healthz",
		HealthTimeoutMs: 10000,
		DrainTimeoutMs:  5000,
		APIPort:         9100,
		Listen:          "127.0.0.1",
		AgentAuth:       "header",
		AgentAuthHeader: "X-Authenticated-User",
		AgentAccess:     "allAuth",
		StartCommand:    "ruby app.rb",
	})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	// 1. The fields that decide who can reach an agent that commits and deploys
	//    are present with real values. Leaving them out would be correct — the
	//    defaults are the same — and would mean a reader has to already know
	//    that to know what they have.
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("scaffold produced invalid JSON: %v\n%s", err, data)
	}
	for field, want := range map[string]string{
		"listen":            "127.0.0.1",
		"agent_auth":        "header",
		"agent_auth_header": "X-Authenticated-User",
		"agent_access":      "allAuth",
	} {
		if got[field] != want {
			t.Errorf("scaffold has %s = %v, want %q", field, got[field], want)
		}
	}

	// 2. Nothing empty. The previous version marshalled the whole struct, so a
	//    new operator met twenty-nine fields of which four mattered — and
	//    `"agent_auth": ""` among them, which reads as a decision to run
	//    without authentication and means the opposite.
	for field, value := range got {
		switch v := value.(type) {
		case string:
			if v == "" {
				t.Errorf("scaffold emits empty %s; an unset field should be absent", field)
			}
		case float64:
			if v == 0 {
				t.Errorf("scaffold emits zero %s; an unset field should be absent", field)
			}
		case nil:
			t.Errorf("scaffold emits null %s; an unset field should be absent", field)
		}
	}
}

// And it has to survive the loader it was written for. A scaffold that does not
// validate is worse than none: it fails at startup, in a config the operator did
// not write and cannot be expected to debug.
func TestScaffoldLoadsAndValidates(t *testing.T) {
	t.Parallel()

	data, err := scaffold(config.Config{
		Port: 3000, InternalPort: 3000, HealthEndpoint: "/healthz",
		HealthTimeoutMs: 10000, DrainTimeoutMs: 5000, APIPort: 9100,
		Listen: "127.0.0.1", AgentAuth: "header",
		AgentAuthHeader: "X-Authenticated-User", AgentAccess: "allAuth",
		StartCommand: "ruby app.rb", SetupCommand: "bundle install",
	})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	path := filepath.Join(t.TempDir(), "slot-machine.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the scaffolded config does not validate: %v\n%s", err, data)
	}
	if cfg.StartCommand != "ruby app.rb" || cfg.SetupCommand != "bundle install" {
		t.Errorf("commands did not survive the round trip: %+v", cfg)
	}

	// The scaffold must not quietly disagree with the loader about the values it
	// bothered to write down.
	if cfg.AgentAccess != "allAuth" || cfg.AgentAuth != "header" || cfg.Listen != "127.0.0.1" {
		t.Errorf("loaded values differ from the scaffolded ones: %+v", cfg)
	}

	// A command containing characters that need escaping must not produce
	// broken JSON, which is the hazard in assembling the document by hand.
	weird, err := scaffold(config.Config{
		Port: 3000, StartCommand: `sh -c "echo \"hi\" && run"`, APIPort: 9100,
	})
	if err != nil {
		t.Fatalf("scaffold with a quoted command: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(weird, &check); err != nil {
		t.Fatalf("scaffold produced invalid JSON for a quoted command: %v\n%s", err, weird)
	}
	if !strings.Contains(check["start_command"].(string), `echo \"hi\"`) {
		t.Errorf("start_command mangled: %v", check["start_command"])
	}
}
