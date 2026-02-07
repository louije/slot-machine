# slot-machine

Blue-green deploys on a single machine. One binary, two slots, zero downtime.

```
slot-machine init       # detect project, generate config
slot-machine start      # start daemon, manage deploys
slot-machine deploy     # deploy HEAD (or a specific commit)
slot-machine rollback   # swap back to previous slot
slot-machine status     # what's running
```

## How it works

The daemon manages two **slots** — independent git worktrees of your app.
One slot is live (serving traffic). The other is the **workspace**: a full
working tree where you — or an agent — can edit code, run tests, commit,
and prepare the next deploy.

```
  ┌──────────────────────────────────────────────────────────────┐
  │                    slot-machine daemon                       │
  │                                                              │
  │   ┌──────────────┐                ┌──────────────┐           │
  │   │   slot A      │◄── live       │   slot B      │  staging │
  │   │   (v3)        │               │   (v4-wip)    │          │
  │   │               │               │               │          │
  │   │  serving :3000 │               │  edit, test,  │          │
  │   │               │               │  commit here  │          │
  │   └───────┬───────┘               └───────────────┘          │
  │           │                                                  │
  └───────────┼──────────────────────────────────────────────────┘
              │
              ▼
         :3000 app
```

When the work is ready, `slot-machine deploy` promotes the staging slot to
live, and the old live slot becomes the new workspace:

```
  ┌──────────────────────────────────────────────────────────────┐
  │                    slot-machine daemon                       │
  │                                                              │
  │   ┌──────────────┐                ┌──────────────┐           │
  │   │   slot A      │  workspace    │   slot B      │◄── live  │
  │   │   (v3)        │               │   (v4)        │          │
  │   │               │               │               │          │
  │   │  edit, test,  │               │  serving :3000 │          │
  │   │  commit here  │               │               │          │
  │   └───────────────┘               └───────┬───────┘          │
  │                                           │                  │
  └───────────────────────────────────────────┼──────────────────┘
                                              │
                                              ▼
                                         :3000 app
```

The slots are git worktrees, so `node_modules`, build artifacts, and data
directories persist across deploys. Rollback re-starts the previous slot
instantly — no re-checkout, no re-install.

## The agent workflow

The inactive slot is designed as a staging area for an AI agent (or a human).
The typical cycle:

```
  ┌─────────┐     ┌──────────────┐     ┌────────────┐     ┌──────────┐
  │  pull    │────▶│  edit & test  │────▶│   commit   │────▶│  deploy  │
  │  main    │     │  in staging   │     │  to machine │     │          │
  └─────────┘     └──────────────┘     └────────────┘     └──────────┘
       ▲                                                        │
       └────────────────────────────────────────────────────────┘
                          staging slot rotates
```

1. The agent works on a `machine` branch in the inactive slot
2. Humans push to `main` on the remote
3. The agent pulls `main` and merges it into `machine`
4. `slot-machine deploy` promotes the result
5. The old live slot is now the workspace — repeat

The live app is never touched. If a deploy fails health checks, the process
is killed and the live slot stays untouched. Rollback is always one command away.

## Deploy sequence

```mermaid
sequenceDiagram
    participant C as Client
    participant D as Daemon
    participant A as Slot A (live)
    participant B as Slot B (staging)

    C->>D: POST /deploy {commit}
    D->>D: Lock (reject concurrent deploys)
    D->>B: git checkout commit
    D->>B: setup_command (bun install, pip install, ...)
    D->>A: SIGTERM → drain
    Note over A: graceful shutdown<br/>or SIGKILL after timeout
    D->>B: start_command
    loop Health check
        D->>B: GET /healthz
        B-->>D: 200 OK?
    end
    alt Healthy
        D->>D: Promote B to live, A becomes workspace
        D-->>C: {success: true, slot: "b"}
    else Unhealthy
        D->>B: SIGKILL
        D-->>C: {success: false}
    end
```

## Quick start

```sh
cd your-app
slot-machine init          # generates slot-machine.json
slot-machine start         # starts daemon on :9100, manages :3000

# in another terminal
slot-machine deploy        # deploy current HEAD
slot-machine deploy abc123 # deploy a specific commit
slot-machine rollback      # swap back
slot-machine status        # check what's live
```

## Configuration

`slot-machine init` detects your project type and generates `slot-machine.json`:

```json
{
  "start_command": "bun server/index.ts",
  "setup_command": "bun install --frozen-lockfile",
  "port": 3000,
  "internal_port": 3000,
  "health_endpoint": "/healthz",
  "health_timeout_ms": 10000,
  "drain_timeout_ms": 5000,
  "env_file": ".env",
  "api_port": 9100
}
```

| Field | What it does |
|-------|-------------|
| `start_command` | How to start your app |
| `setup_command` | Runs after checkout, before start (e.g. install deps) |
| `port` | The port your app listens on (injected as `PORT` env var) |
| `internal_port` | Health check port (injected as `INTERNAL_PORT`) |
| `health_endpoint` | Path to poll for 200 OK |
| `health_timeout_ms` | How long to wait for healthy before giving up |
| `drain_timeout_ms` | Graceful shutdown window before SIGKILL |
| `env_file` | Loaded into the app's environment (secrets, DB paths) |
| `api_port` | Daemon API port for deploy/rollback/status |

Project detection: `bun.lock` → Bun, `package-lock.json` → npm,
`requirements.txt` → pip, `Gemfile.lock` → Bundler.

## The spec

The `spec/` directory defines the slot-machine contract as an executable test
suite. It treats the binary as a black box — all interaction is through the
CLI and HTTP API. The current spec version is in `spec/VERSION`.

**17 scenarios:**

| # | Test | What it validates |
|---|------|-------------------|
| 1 | `TestDeployHealthy` | Deploy succeeds, app responds, status correct |
| 2 | `TestDeployUnhealthy` | Failed health check → process killed, no live slot |
| 3 | `TestDeployThenRollback` | A → B → rollback → A is live again |
| 4 | `TestOnlyOnePreviousSlot` | Only the immediately prior deploy is kept |
| 5 | `TestRollbackNoPrevious` | Rollback with nothing deployed → error |
| 6 | `TestConcurrentDeployRejected` | Second deploy during first → 409 |
| 7 | `TestProcessCrashDetected` | Crash sets healthy=false |
| 8 | `TestDrainTimeoutForceKill` | SIGTERM timeout → escalate to SIGKILL |
| 9 | `TestNoArgs` | No args → usage message, exit 1 |
| 10 | `TestUnknownCommand` | Bad subcommand → error |
| 11 | `TestStartMissingConfig` | No config → helpful error |
| 12 | `TestInitBunProject` | Detects bun project, correct config |
| 13 | `TestInitAppendsGitignore` | `.slot-machine` added to `.gitignore`, idempotent |
| 14 | `TestDeployNoRunningDaemon` | Deploy without daemon → connection error |
| 15 | `TestEnvFilePassedToApp` | Env file vars available in the app |
| 16 | `TestSetupCommandRuns` | Setup command executes before start |
| 17 | `TestDaemonShutdownDrainsProcesses` | SIGTERM to daemon → child processes cleaned up |

```sh
# build
go build -o slot-machine ./cmd/slot-machine/
go build -o spec/testapp/testapp ./spec/testapp/

# run the spec
ORCHESTRATOR_BIN=$(pwd)/slot-machine go test -v -count=1 ./spec/
```
