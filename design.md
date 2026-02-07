# Self-Deploying Agent Architecture

*Draft — February 2026*

An architecture for web applications that embed a coding agent capable of modifying, deploying, and rolling back its own code, with safe concurrency between human and machine contributors.

See also:
- [Orchestrator Spec](orchestrator-spec.md) — the minimal, reusable interface to build against
- [Migration Policy](migration-policy.md) — how database schema changes are handled safely

---

## 1. System Overview

The system allows a coding agent embedded in a web app to modify the app's own source code and deploy changes with zero downtime. It uses a blue-green deployment pattern managed by a per-app controller (the orchestrator), with Git as the source of truth and GitHub as the remote sync point.

There is no build step. The app runs directly from source. Server restarts may be required for backend changes, so the orchestrator manages process lifecycle, health checking, and traffic routing.

## 2. Core Components

### Reverse Proxy

A shared proxy (nginx, Caddy) that routes incoming traffic to the currently live port for each app. The orchestrator updates the proxy configuration on each successful swap. This is the only shared infrastructure across apps.

### Orchestrator (one per app)

A minimal, long-lived process that manages the deploy lifecycle for a single app. It owns the proxy routing for its app, the lifecycle of both deploy slots, health checking, rollback, and Git operations. The agent communicates with it through a constrained API — it never manipulates processes or the proxy directly.

The orchestrator exposes a small API:

```
POST /deploy    — trigger a deploy from a commit or branch
POST /rollback  — revert to the previous live slot
GET  /status    — current live commit, slot states, recent deploy log
```

See [Orchestrator Spec](orchestrator-spec.md) for the full interface definition and validation suite.

### Deploy Slots

Two directories on disk, alternating blue-green style: `deploy-a/` and `deploy-b/`. Each is a complete working copy of the app tied to a specific Git commit. The orchestrator writes to the inactive slot and swaps on successful promotion. The previously live slot is kept intact for fast rollback.

The two directories exist so that the old instance can keep serving traffic while the new one boots. This is not an archival strategy — Git is the archive. The second slot is just the fast escape hatch for instant rollback.

### Coding Agent

The agent writes code to the inactive deploy slot, commits to the `machine` branch, and requests a deploy through the orchestrator API. It is responsible for pulling and merging the human branch before starting work. It has no direct access to process management, the proxy, or the orchestrator's own code.

### GitHub

The remote source of truth. Human developers work on `main`. The agent's commits are pushed to `machine`. A webhook notifies the orchestrator when `main` is updated, triggering a deploy through the same pipeline the agent uses.

## 3. Deploy Flow

Both agent-initiated and human-initiated deploys follow the same path. The orchestrator does not distinguish between sources — it receives a commit and runs the same promotion sequence.

```
1. Write files to inactive slot
2. Git commit (captures intent)
3. Pre-commit gate (secrets, diff size, protected paths)
4. Boot new instance on staging port
5. Deep health check
   ├─ HEALTHY:   6a. Swap proxy → 7a. Drain & stop old instance
   │             8. Push to GitHub → 9. Update deploy status
   └─ UNHEALTHY: 6b. Kill failed instance → 7b. Log failure, alert, no swap
```

The commit happens before the health check, not after. This captures intent: if the health check fails, the commit remains in the branch as a record of a failed attempt, which is useful for debugging. The orchestrator tags which commit is actually live separately.

Connection draining is built into the swap. The orchestrator tells the proxy to stop routing new requests to the old instance, waits for in-flight requests to complete (up to a configurable timeout), then stops the old process.

## 4. Branch Model and Git Sync

Humans and machines write to separate branches. This eliminates merge conflicts during deploys and makes authorship clear in the commit history.

### Rules

- `main` is the human branch. Humans push here. CI runs here.
- `machine` is the agent's branch. The agent commits and pushes here.
- The agent is responsible for merging `main` into `machine` before starting work. If the merge conflicts, the agent resolves it — an LLM-based agent is well-suited to this since it understands the codebase contextually.
- Humans accept agent changes by merging `machine` into `main` through a pull request, with optional review.
- Either branch can be deployed. The orchestrator tracks which commit is live regardless of branch.

### Tracking the live commit

The orchestrator calls the GitHub Deployments API after each successful swap, marking the promoted commit as the active deployment. A `GET /status` endpoint on the orchestrator also returns the current live commit hash for programmatic access.

### Inbound webhook handling

When a push to `main` arrives via webhook, the orchestrator deploys it immediately through the standard flow. The agent's working tree is now behind, but that's the agent's responsibility to resolve before its next task. If the agent is mid-edit when the webhook fires, the orchestrator deploys the human version anyway — the agent will pull and merge when it's ready.

### Webhook reliability

GitHub webhooks are not guaranteed delivery. A reconciliation loop in the orchestrator polls every few minutes: is `main` ahead of what's deployed? This catches missed webhooks without adding complexity to the happy path.

### Git hygiene

An active agent can produce hundreds of commits per day. When the agent's branch is merged into `main`, squash it. Post-merge, the orchestrator resets `machine` to `main`'s HEAD, giving the agent a clean starting point. The orchestrator runs `git gc` periodically on the local repo.

## 5. Continuous Validation

Traditional CI doesn't map cleanly onto this system. The agent needs sub-second feedback, not a minutes-long remote pipeline. Instead, the orchestrator performs *continuous validation* — a lightweight, ongoing verification layer that runs at deploy time and between deploys.

This is not CI replacement. It's the layer in between integration and deployment that asks "is this safe to make live" — continuously, every time something changes. The reconciliation loop means the system is re-validating state even between deploys: checking that what's running matches what's committed, that branches haven't drifted too far, that the health contract still holds. Validation as an ongoing property of the system rather than a one-time event in a pipeline.

### At deploy time

- **Pre-commit gate.** Before accepting a deploy, the orchestrator scans the diff for secret patterns, checks for modifications to protected paths, and enforces a diff size limit. This runs in the orchestrator, not in Git hooks (which the agent could bypass).
- **Deep health check.** Not just "does the process respond" but "can it serve a representative request." The orchestrator sends a synthetic request that exercises actual app logic — touches the database, renders a template, whatever the app does. The health check contract is defined in the app contract file.
- **Optional smoke test.** The agent can maintain a smoke test file that the orchestrator runs as part of promotion. This catches regressions the health endpoint wouldn't surface.

### Between deploys (reconciliation loop)

Every few minutes, the orchestrator checks:

- Is the human branch ahead of what's deployed?
- Has the agent's branch diverged from the human branch by more than N commits?
- Is the running process still healthy?
- Is disk usage above a threshold?
- Have there been consecutive failed deploys?

### What stays in traditional CI

Full test suites, linting, and comprehensive checks still run on GitHub Actions for the human branch. The orchestrator doesn't replace CI — it handles the deploy-time validation that needs to be local and fast. Agent commits can optionally be pushed to GitHub for async CI validation that doesn't block the deploy.

## 6. App Contract

Each app has a contract file that lives outside the agent's writable scope. The orchestrator owns it and injects it. The agent can read it to understand its constraints but cannot modify it.

```json
{
  "start_command": "node server.js",
  "port": 3001,
  "internal_port": 3901,
  "health_endpoint": "/healthz",
  "health_test_payload": { "method": "GET", "path": "/api/ping" },
  "health_timeout_ms": 5000,
  "drain_timeout_ms": 10000,
  "schema_status_endpoint": "/schema/status",
  "protected_paths": [
    ".env",
    "app.contract.json",
    "orchestrator/"
  ],
  "secrets_inject": ["DATABASE_URL", "API_KEY", "SESSION_SECRET"],
  "max_deploy_size_mb": 50,
  "max_diff_lines": 2000,
  "alert_webhook": "https://hooks.slack.com/services/..."
}
```

This solves several problems in one place: the orchestrator knows how to manage the app without inspecting its code, the agent can't drift the integration points, and secrets are injected from outside rather than living in the codebase.

The `internal_port` is separate from the public-facing `port`. Health checks and schema status are served on the internal port, which is not exposed through the proxy. See [Internal Communication and Security](#7-internal-communication-and-security).

## 7. Internal Communication and Security

The orchestrator and app communicate over endpoints that are never exposed to the internet. The app listens on two interfaces:

- **Public port** (e.g. `3001`) — serves actual user traffic through the reverse proxy.
- **Internal port** (e.g. `3901`) — serves health checks, schema status, and app-managed operations like migrations. Bound to `localhost` only. The proxy never routes to this port.

This means no TLS, no auth tokens, no API keys to rotate for internal communication. The orchestrator calls `http://localhost:3901/healthz` and that's it. Nothing outside the machine can reach it.

### Defense against the agent

The realistic threat model isn't a sophisticated attacker — it's the agent accidentally writing code that calls its own internal endpoints and triggers a migration or corrupts health checks. Two layers of defense:

- **Unix domain sockets** (optional hardening). Instead of a TCP port, the orchestrator and app communicate over `/var/run/app-a/orchestrator.sock`. The socket file is owned by the orchestrator's user with restricted permissions. The app process runs as a different user that can listen on the socket but the agent's code can't easily discover or abuse it.
- **Separate user for gated operations.** The app's internal endpoints (e.g. `/migrations/run`) are the app's own responsibility. Access control is handled by the internal port being localhost-only and, optionally, by the Unix socket's file permissions. The orchestrator never calls the migration endpoint — it only reads `/schema/status`. Whoever triggers migrations (human, CLI tool, or the agent through a controlled path) does so directly against the app.

> Defense in depth matters less here than keeping the attack surface small. Localhost-only internal ports with a separate user for the orchestrator process covers the practical risks without adding infrastructure.

## 8. Agent Lifecycle

The agent is triggered by a chat UI in the app. A user sends a message, the app spawns or calls the agent, and the agent does its work. This makes the agent a subprocess of the app — but introduces a lifecycle problem.

### The swap problem

The app in slot A spawns the agent. The agent writes to slot B, calls deploy, the orchestrator swaps traffic to slot B, drains and stops slot A — killing the agent that initiated the whole thing.

### Solution: detached agent process

The agent process must not be a direct child of the app's server process. The app spawns it as a **detached background process** that isn't tied to the app's lifecycle. The user gets updates via polling or websocket. When the app swaps, the new slot picks up the connection (or the client reconnects). The orchestrator's drain logic knows: stop accepting requests, wait for in-flight requests, but **don't touch the agent process** — it finishes on its own.

### Alternative: orchestrator-managed agent

The app could forward the task to the orchestrator, which spawns the agent itself — since the orchestrator is the one thing that survives deploys. This keeps the lifecycle clean but couples the orchestrator to the agent's existence, violating the "keep the orchestrator minimal" principle. Rejected for v1, but worth revisiting if the detached-process approach proves fragile.

## 9. Multi-App Topology

When multiple apps on the same server use this pattern, each gets its own orchestrator. A single shared reverse proxy routes traffic to all of them, and a small registry tracks which apps exist and their current status.

This keeps failure domains separate. A bad deploy in App A doesn't risk App B's orchestration. Each app can have its own deploy cadence, health check logic, and rollback policy. The shared proxy is the only coupling point, and it's deliberately simple — it just maps hostnames or paths to ports.

## 10. App Constraints

The architecture assumes apps are written to be stateless at the process level. This is a hard requirement, not a nice-to-have — without it, the blue-green swap loses data or breaks user sessions.

- **No in-process state that matters.** Sessions, caches, and job queues live in external stores (Redis, database, filesystem). Each instance is disposable.
- **Shared state is mounted, not copied.** Database connections, uploaded files, and config/secrets are symlinked or injected from a common location. Deploy slots contain only code.
- **Graceful shutdown.** The app must handle SIGTERM by finishing in-flight requests and exiting cleanly within the drain timeout.
- **Deterministic startup.** The app boots and passes its health check within a predictable window. No lazy initialization that only triggers on the first real request.

> These are the same constraints you'd apply to any load-balanced or containerized application. The blue-green swap is simpler than a typical load-balanced setup because only one instance serves traffic at a time — you don't need to worry about two instances seeing inconsistent state simultaneously. You just need to make sure the new instance can pick up where the old one left off.

## 11. Container Adoption Criteria

Containers are not part of the v1 architecture, but the orchestrator pattern stays the same if you adopt them later — you're just swapping "start a process" for "start a container." The decision is about when the complexity earns its keep.

**Where containers help:**
- **Isolation.** If the agent generates pathological code (eats memory, writes to unexpected paths, spawns runaway processes), a container gives you cgroups/resource limits and filesystem isolation for free.
- **Reproducibility.** Each deploy slot becomes an image tagged with its commit hash. Rollback is just running the previous image.
- **Clean teardown.** Stopping an old instance is `docker stop` rather than hoping your process manager caught every child process.

**Where they don't add much:**
- **Build step.** Containers introduce one. For frequent small agent changes, that friction adds up.
- **Debugging.** Inspecting running state is simpler with files on disk and a process than with a container layer.
- **Redundancy.** The blue-green proxy swap already provides zero-downtime deploys without containers.

**Adoption threshold** — consider containers when any of these become true:
- The app runs on multiple machines.
- The agent's code becomes untrusted enough that real sandboxing is needed.
- Resource limits via `ulimits`/cgroups are insufficient and you need full filesystem isolation.

## 12. Risks and Mitigations

| Risk | Severity | Mitigation |
|------|----------|------------|
| **Database migrations** — agent changes code that expects a new schema | High | Migrations decoupled from deploys. Schema compatibility as version ranges. Orchestrator checks `/schema/status` before promotion and rollback. See [Migration Policy](migration-policy.md). |
| **Agent modifies integration points** — changes the port, health endpoint, or orchestrator API calls | High | App contract file is outside the agent's writable scope. Orchestrator injects config rather than reading it from app code. |
| **Secret exposure** — agent accidentally commits credentials | High | Secrets injected by orchestrator from external store. Pre-commit gate scans diffs for secret patterns. Agent has no direct access to secret store. |
| **Agent calls internal endpoints** — agent-written code triggers migrations or corrupts health checks | High | Internal endpoints on localhost-only port. Optional Unix domain socket with restricted permissions. |
| **Agent killed mid-task by deploy swap** | Medium | Agent runs as detached process. Orchestrator's drain logic excludes the agent. |
| **Stateful drift** — in-memory sessions, caches, websocket connections lost on swap | Medium | App constraint: externalize all state. Connection draining before stopping old instance. |
| **Cascading failed deploys** — flawed health check causes valid deploys to fail repeatedly | Medium | Orchestrator tracks consecutive failures and alerts after a threshold. Manual override available. |
| **Disk pressure** — deploy directories and git history accumulate | Medium | Only current and previous slots kept. Stale slots cleaned up. Git gc runs periodically. Agent commits squashed on merge. |
| **Webhook failure** — GitHub webhook missed, human deploy doesn't propagate | Medium | Reconciliation loop polls remote branch every few minutes. |
| **Git history bloat** — agent produces hundreds of commits per day | Low | Agent branch squashed on merge to main. Machine branch resets to main's HEAD post-merge. |
| **Orchestrator itself needs updating** | Low | Keep minimal. Test separately. Agent cannot modify orchestrator code. |

### Alerting

The orchestrator posts to a configured webhook (Slack, Discord, email relay) on consecutive failed deploys, merge conflicts the agent couldn't resolve, disk pressure, and health check timeouts. The goal is that the orchestrator never silently gives up.

### Deploy log

Every deploy attempt — successful or not — is recorded in an append-only log with the commit hash, source branch, timestamp, health check result, promotion status, and trigger source (agent, webhook, manual). This serves as the audit trail and powers any future dashboard.
