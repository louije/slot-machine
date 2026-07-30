package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Migration policy — docs/migration-policy.md.
//
// The orchestrator never connects to the database, never parses SQL and never
// runs a migration. It reads one JSON document from the app's internal port and
// makes a pass/fail decision, exactly like the health check. Everything else
// about migrations is the app's business.
//
// This closes the asymmetry that makes agent-authored deploys risky: a code
// deploy is reversible in seconds, a migration is not. Code that has moved
// outside the schema's compatible range must not be promoted, and — the case
// that actually bites — code must not be *rolled back* to a version that
// predates a migration which has already run.

type schemaStatus struct {
	CurrentSchemaVersion *int     `json:"current_schema_version"`
	CodeMinSchemaVersion *int     `json:"code_min_schema_version"`
	CodeMaxSchemaVersion *int     `json:"code_max_schema_version"`
	PendingMigrations    []string `json:"pending_migrations"`
	Compatible           *bool    `json:"compatible"`
}

// checkSchemaCompatible probes the candidate slot's schema status. The slot is
// already booted and healthy at this point, so this costs one local request.
//
// The same check serves deploy and rollback. The endpoint is served *by the slot
// being probed*, so it reports that code's supported range alongside the
// database's actual version — which is precisely the rollback question ("does
// this older code still understand the schema we now have?") as well as the
// deploy one.
func (o *orchestrator) checkSchemaCompatible(s *slot) error {
	if o.cfg.SchemaStatusEndpoint == "" {
		return nil
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", s.intPort, o.cfg.SchemaStatusEndpoint)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("schema status: cannot reach %s: %v", o.cfg.SchemaStatusEndpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("schema status: reading response: %v", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("schema status: %s returned %d (schema_status_endpoint is configured, so the app is expected to serve it)",
			o.cfg.SchemaStatusEndpoint, resp.StatusCode)
	}

	var st schemaStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return fmt.Errorf("schema status: response is not valid JSON: %v", err)
	}

	// An explicit false is authoritative — the app knows things we do not.
	if st.Compatible != nil && !*st.Compatible {
		detail := ""
		if len(st.PendingMigrations) > 0 {
			detail = fmt.Sprintf(" (pending migrations: %v)", st.PendingMigrations)
		}
		return fmt.Errorf("schema status: the app reports it is not compatible with the current schema%s", detail)
	}

	// Independently verify the range when the app supplies the numbers, so a
	// hard-coded `"compatible": true` is not the only thing standing between a
	// rollback and a schema it cannot read.
	if st.CurrentSchemaVersion == nil {
		return nil
	}
	current := *st.CurrentSchemaVersion

	if st.CodeMinSchemaVersion != nil && current < *st.CodeMinSchemaVersion {
		return fmt.Errorf(
			"schema status: this code needs schema version %d or newer but the database is at %d — run the migration before deploying",
			*st.CodeMinSchemaVersion, current)
	}
	if st.CodeMaxSchemaVersion != nil && current > *st.CodeMaxSchemaVersion {
		return fmt.Errorf(
			"schema status: this code supports schema version %d at most but the database is at %d — it cannot read the current schema",
			*st.CodeMaxSchemaVersion, current)
	}
	return nil
}
