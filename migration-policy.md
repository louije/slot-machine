# Migration Policy

Code deploys are reversible — swap back to the previous slot in seconds. Database migrations are not. This asymmetry is the single most dangerous aspect of agent-authored deploys.

## What Goes Wrong

| Scenario | Consequence |
|----------|-------------|
| Agent adds a column, rollback happens | Harmless — old code ignores extra columns. But orphaned schema changes accumulate. |
| Agent renames or drops a column | **Breaking.** Old code reads a column that no longer exists. Everything fails. |
| Agent changes a column type (e.g. integer to string) | Old code does arithmetic on a string. Silent data corruption or runtime errors. |
| Agent adds NOT NULL without default | Migration fails partway. Schema is in a partially migrated state that neither code version expects. |
| Two deploys in quick succession, each with a migration | Rollback one deploy doesn't undo both migrations. Schema/code mismatch. |
| Human and agent both write migration 003 | Conflicting migration numbers. One gets skipped or they run in unpredictable order. |

## Why Reversible Migrations Aren't Enough

Rails-style migrations work well when a human runs `rake db:migrate`, checks the result, and runs `rake db:rollback` if needed. It's a supervised process. Here, the agent writes the migration, runs it, and the rollback trigger might come hours later — after new data has been written against the new schema. A reverse migration that drops an added column destroys any data written to that column while the new code was live.

The bigger issue: Rails-style rollback assumes you're rolling migrations back in order, one at a time. In the blue-green model, a rollback means "switch traffic to the old slot instantly." The database migration rollback is a separate, slower operation. There's always a window where old code runs against the new schema.

## The Solution: Decouple Migrations from Deploys

Migrations do not run at deploy time. They are a separate, explicitly gated operation.

- The agent can *write* migration files and commit them like any other code. They go through the same pre-commit gate.
- The orchestrator never runs migrations. It doesn't even know how. Running a migration is a direct operation on the app — a human, a CLI tool, or the agent calls the app's `/migrations/run` endpoint on its internal port. The orchestrator is not in this loop.
- This forces code to be written in a way that works with both the old and new schema — because there's always a window between deploy and migration.

## Schema Compatibility as a Version Range

The app exposes a `/schema/status` endpoint on its internal port that reports compatibility:

```json
{
  "current_schema_version": 42,
  "code_min_schema_version": 41,
  "code_max_schema_version": 43,
  "pending_migrations": ["043_add_invoice_notes.sql"],
  "compatible": true
}
```

The key fields are `code_min_schema_version` and `code_max_schema_version`. The developer (or agent) declares: "this version of the app works with schema versions 41 through 43." If the database is at version 42 and the code says it works with 41–43, that's compatible. If a rollback target only supports 38–40, the orchestrator blocks the rollback.

The agent writes these version bounds when it writes code changes. If it adds a migration that creates a column but the code doesn't strictly require it yet, `code_min_schema_version` stays put. If it writes code that depends on the new column, `code_min_schema_version` moves up — and at that point the orchestrator won't promote the deploy until the migration has actually run.

## What the Orchestrator Does and Doesn't Do

The orchestrator never connects to the database. It never parses SQL. It never runs migrations. It reads JSON from `/schema/status` and makes pass/fail decisions — the same pattern as the health check:

- **Before promoting a deploy:** call `/schema/status` on the new slot. If `compatible` is false, don't promote.
- **Before allowing a rollback:** boot the target slot on a temp port and call `/schema/status`. If its `code_max_schema_version` is below the current schema version, block the rollback.

That's it. The orchestrator's involvement with migrations is purely observational — it checks compatibility, it doesn't cause state changes. Running migrations, choosing when to run them, and handling failures are the app's responsibility.

## Where Migrations Live in the Repo

In the repo, migrations are a flat sequence — `migrations/001_create_users.sql`, `migrations/002_add_billing.sql`, etc. — like any standard migration framework. The repo doesn't care about pending vs applied. That's runtime state, tracked in the database's own `schema_migrations` table (or equivalent). The repo is the source of truth for what migrations exist. The database is the source of truth for which ones have run.

> This approach doesn't rely on hoping the agent writes correct expand/contract migrations. The orchestrator *tests* whether each deploy and rollback is safe against the current schema. The agent can write whatever it wants — the system just won't promote or rollback to code that's incompatible with the database.
