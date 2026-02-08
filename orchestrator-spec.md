# Orchestrator Spec

The orchestrator is a process that manages deploys on a single machine. Its core knows about slots, processes, health checks, and a proxy. The agent service and chat UI are built on top — see [agent.md](agent.md).

This pattern doesn't exist as a standalone tool. Cloud-native orchestration (Kubernetes, ECS) handles it at scale. On a single machine, people reach for pm2 or a shell script. The middle ground — a proper spec with a validation suite that anyone can implement — is the gap.

## Interface

```
POST /deploy {commit: string}
  → checks out commit into inactive slot
  → boots the process on the slot's internal port
  → calls the health endpoint
  → if healthy: swaps the proxy, drains the old process
  → if unhealthy: kills the new process, no swap
  → returns {success, slot, commit, previous_commit}

POST /rollback
  → if the previous slot still has a running or bootable process:
    → boots it if needed, health checks it
    → swaps the proxy, drains the current process
  → returns {success, slot, commit}

GET /status
  → returns {live_slot, live_commit, previous_slot,
     previous_commit, last_deploy_time, healthy}
```

## App Contract

The orchestrator needs this from the app — nothing more:

```json
{
  "start_command": "node server.js",
  "port": 3001,
  "internal_port": 3901,
  "health_endpoint": "/healthz",
  "health_timeout_ms": 5000,
  "drain_timeout_ms": 10000
}
```

The full app contract (in [design.md](design.md)) extends this with fields for schema status, protected paths, secrets injection, diff limits, and alerting. Those are concerns layered on top — the orchestrator's core only needs the fields above.

## Validation Suite

Any implementation of the orchestrator can be validated against these scenarios:

1. Deploy a commit. Health check passes. Verify traffic routes to new slot.
2. Deploy a commit. Health check fails. Verify traffic stays on old slot.
3. Deploy, then rollback. Verify traffic returns to previous slot.
4. Deploy twice. Verify only one previous slot is retained.
5. Rollback with no previous slot. Verify error.
6. Deploy while a deploy is in progress. Verify queued or rejected.
7. Process crashes after promotion. Verify health status reflects it.
8. Drain timeout exceeded. Verify old process is force-killed.

The spec is small enough to implement in ~500 lines of any language. The test suite validates behavior regardless of implementation.

## Implementation Notes

The orchestrator needs to be a single static binary with no runtime dependencies — it sits alongside apps that could be Node, Python, Ruby, and shouldn't compete for the same runtime.

- **Go** — single-binary, fast development cycle. The standard library covers everything needed (HTTP server, process management, filesystem, JSON). Better for a v1 you want to build quickly and iterate on.
- **Rust** — maximally minimal, no GC, rock-solid long term. Better for the "drop a binary on any server and forget about it" story.
