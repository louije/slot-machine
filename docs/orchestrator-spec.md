# Orchestrator Spec

An orchestrator manages zero-downtime deploys of one application on one machine.
It owns the app's public port, checks a commit out somewhere, starts it, decides
whether it is healthy, and only then moves traffic. Rollback returns to the
version that was live before.

This document is normative and self-contained: an implementation should be
writable from it alone, without reading any existing one. Every requirement is
numbered (**R1**, **R2**, …) and every requirement has at least one test in
`spec/conformance/`, checked mechanically — see [Conformance](#conformance).

The pattern doesn't exist as a standalone tool. Cloud-native orchestration
(Kubernetes, ECS) handles it at scale. On a single machine, people reach for pm2
or a shell script. The middle ground — a spec with a suite that anyone can
implement against — is the gap.

Two implementations exist in this repository: `cmd/slot-machine` (Go, and the one
with features beyond this spec) and `impl/ruby` (Ruby, which implements only this
document). Both pass the same suite. That is deliberate: a suite that has only
ever run against one implementation encodes that implementation's assumptions
invisibly.

---

## 1. Startup and lifecycle

An orchestrator is started with four things: the path to an app contract, the
app's repository, a directory it may write to, and the port to serve its own API
on. How they are supplied is the implementation's business — the conformance
suite reaches an implementation through a small adapter script, so a CLI of any
shape is fine.

**R1.** The API answers `GET /` with `200` once the orchestrator is ready to
accept requests.

> This is the readiness signal. Nothing else in the API is guaranteed to answer
> before a deploy has happened, so a caller has no other way to know the
> orchestrator is up. The body is not specified.

**R2.** An orchestrator MAY do work at startup — including deploying the
repository's `HEAD` — before answering `R1`. If it does, `R1` MUST NOT answer
until that work has finished.

> So that "the API answers" is a usable signal for "startup is done". An
> implementation that serves during startup would make every caller race it.

**R3.** The orchestrator MUST bind the app's public port (`port`) at startup,
before any deploy, and MUST keep it bound for its lifetime.

> The public port belongs to the orchestrator, not to any app process. This is
> what allows the process behind it to be replaced without the port going away,
> and it is why an app whose very first deploy fails still has a reachable port
> to report the failure on.

**R32.** The orchestrator's own API MUST NOT be reachable from outside the host.

> `POST /deploy` and `POST /rollback` carry no credential — §8 puts
> authentication out of scope, and the CLI most implementations ship is a thin
> client over these endpoints. The bind address is therefore the only thing
> between a promotion and everyone else on the network, which makes it a
> requirement rather than a deployment note.
>
> This was written as an expectation in §8 and honoured by neither prose nor
> code: the Go implementation bound every interface while its own documentation
> claimed localhost, and the Ruby one bound loopback. Two conformant
> implementations disagreed about the network boundary, and the suite could not
> see it, because it only ever connected over loopback. An expectation no test
> can fail is a preference.

**R4.** On `SIGTERM` the orchestrator MUST stop the app processes it started and
exit.

## 2. The app contract

A JSON document describing the app. Only these fields are required of a
conformant implementation; anything else in the file MUST be ignored rather than
rejected, so that one file can carry an implementation's own extensions.

```json
{
  "start_command": "node server.js",
  "port": 3001,
  "internal_port": 3901,
  "health_endpoint": "/healthz",
  "health_timeout_ms": 5000,
  "drain_timeout_ms": 10000,
  "setup_command": "npm ci"
}
```

| Field | Required | Meaning |
|---|---|---|
| `start_command` | yes | Shell command that runs the app in the foreground |
| `port` | yes | The app's stable public port, owned by the orchestrator |
| `health_endpoint` | yes | Path polled to decide whether a version is healthy |
| `internal_port` | no, defaults to `port` | The app's stable internal port |
| `health_timeout_ms` | no, default 10000 | How long a new version may take to become healthy |
| `drain_timeout_ms` | no, default 5000 | How long a retiring process may take to exit |
| `setup_command` | no | Shell command run in the slot before the app starts |

**R5.** `start_command` MUST be run with the slot's directory as its working
directory.

**R6.** The orchestrator MUST allocate a fresh pair of ports for each boot and
pass them to the app as the environment variables `PORT` (public traffic) and
`INTERNAL_PORT` (the health endpoint).

> This is the only channel by which an app can learn where to listen, and it is
> why the ports must be per-boot rather than fixed: the retiring and arriving
> processes are alive at the same time, so they cannot share a port.

**R7.** The health endpoint MUST be polled on the app's `INTERNAL_PORT`, and a
version is healthy when it answers `200`.

**R8.** Public traffic arriving on `port` MUST be forwarded to the live app's
`PORT`, and MUST NOT reach its `INTERNAL_PORT`.

> The internal port carries whatever an app does not want on the internet —
> health, schema status, administrative endpoints. Exposing it through the public
> port would publish all of it.

**R9.** If `internal_port` differs from `port`, requests arriving on
`internal_port` MUST be forwarded to the live app's `INTERNAL_PORT`.

> Otherwise the field addresses nothing. A fixed internal port is how an operator
> or a sibling process reaches the health or schema endpoint of whatever version
> happens to be live.

**R10.** If `setup_command` is present it MUST be run in the slot's directory,
after the commit is checked out and before `start_command`.

## 3. `POST /deploy`

Request body: `{"commit": "<git commit-ish>"}`.

Response body: `{"success": bool, "slot": string, "commit": string, "previous_commit": string}`.

**R11.** The orchestrator MUST resolve the requested commit-ish against the
repository. If it does not resolve, the deploy MUST be refused with `success:
false` and a non-`2xx` status, and nothing about the live version may change.

**R12.** A request with a missing or empty `commit` MUST be refused with
`success: false` and a non-`2xx` status.

**R13.** On success the response MUST report `success: true`, the resolved commit
in `commit`, a non-empty `slot`, and the commit that was live beforehand in
`previous_commit` (empty if nothing was live).

**R14.** The deploy sequence MUST be: check the commit out into a directory no
running version is using; run `setup_command` if present; start the app; poll
the health endpoint until it answers `200` or `health_timeout_ms` elapses.

**R15.** If the new version becomes healthy, the orchestrator MUST switch traffic
to it and then retire the previously live process (§5).

**R16.** If the new version does not become healthy within `health_timeout_ms`,
the orchestrator MUST stop it, MUST leave the live version serving, and MUST
answer `success: false` with a non-`2xx` status.

**R17.** A failed deploy MUST NOT change `live_commit`.

**R18.** While a deploy is in progress, a second `POST /deploy` MUST either be
rejected with `success: false` and a non-`2xx` status, or be queued and run after
the first completes. Two deploys MUST NOT proceed concurrently.

> Both are permitted because both are defensible: rejecting tells the caller
> immediately, queueing means a caller need not retry. What is not permitted is
> two deploys racing over which version is live.

**R19.** A refused deploy SHOULD report a human-readable reason in an `error`
field, and MAY name the stage it stopped at in a `stage` field.

> Not required, because an implementation can conform without it. Strongly
> recommended anyway: a bare `success: false` gives an operator nothing to act
> on, and debugging one during this spec's development cost two reproduction
> cycles before the reason was added.

## 4. `POST /rollback`

Response body: `{"success": bool, "slot": string, "commit": string}`.

**R20.** Rollback MUST return traffic to the version that was live immediately
before the current one. If there is no such version, it MUST be refused with
`success: false` and a non-`2xx` status.

**R21.** The rollback target MUST be held to the same health contract as a
deploy: started if it is not running, polled, and promoted only when healthy.

> A rollback that skipped the check could take an app from broken to absent.

**R22.** Exactly one previous version needs to be retained. A second consecutive
rollback MUST NOT reach two deploys back.

> An implementation may keep more on disk; it must not make them reachable by
> rolling back repeatedly, because then "the previous version" would be
> ambiguous.

## 5. Retiring a process

**R23.** A retiring process MUST first be asked to exit with `SIGTERM`.

**R24.** If it has not exited within `drain_timeout_ms`, it MUST be killed with
`SIGKILL`.

> An app that ignores `SIGTERM` must not be able to hold a deploy open for ever.

**R25.** Traffic MUST be switched to the new version before the old process is
asked to exit, so that no request is sent to a process that is shutting down.

## 6. `GET /status`

**R26.** `GET /status` MUST answer `200` with an object containing exactly these
keys:

```json
{
  "live_slot": "slot-003-a3f2b1c4",
  "live_commit": "a3f2b1c4...",
  "previous_slot": "slot-002-7c36e8d2",
  "previous_commit": "7c36e8d2...",
  "last_deploy_time": "2026-07-30T09:12:44Z",
  "healthy": true
}
```

**R27.** String fields MUST be present as empty strings, not `null` or absent,
when there is nothing to report. `healthy` MUST be a boolean.

> So that a client can read them without a special case per implementation.

**R28.** `last_deploy_time` MUST be RFC 3339 when a deploy has happened, and an
empty string when none has.

**R29.** `healthy` MUST be `true` only while a live version's process is running
and MUST become `false` if it exits.

**R30.** An implementation MAY add further keys. A client MUST NOT be required to
understand them.

## 7. When nothing is live

**R31.** When no version is live — before the first successful deploy, or after
the live process has exited — requests on the public port MUST be answered with
`503`. The port MUST NOT be closed and connections MUST NOT be refused.

> A refused connection is indistinguishable from the machine being gone, which is
> the difference between a client retrying and someone being paged. Releasing the
> port also lets something else on the machine claim it while the app is down,
> so the next deploy has to win a race to get it back.

## 8. Not specified

Deliberately left to the implementation. A conformance test that depends on any
of these is a bug in the test.

- **How a commit becomes a directory.** The Go implementation uses git
  worktrees; the Ruby one uses `git archive` and has no `.git` in a slot at all.
- **Slot naming, and the layout of the data directory.** Slot names appear in
  `GET /status` and are opaque to clients. Nothing else on disk is part of the
  contract: no symlinks, no journal, no log locations.
- **How many old slots are kept on disk**, as long as `R22` holds.
- **Restart recovery.** Whether an orchestrator can resume ownership of a live
  app after being restarted itself is out of scope.
- **Authentication on the API.** Out of scope. `R32` requires the API to be
  unreachable from off-host, which is what makes an unauthenticated API
  defensible; how an operator then reaches it — SSH, a tunnel — is their
  business.
- **The bind address of the app's public port.** `R3` requires it bound;
  where it is bound depends on whether anything fronts it. `R32` covers only the
  orchestrator's own API, which nothing should front.
- **Anything about a coding agent** — see [agent.md](agent.md). The Ruby
  implementation has none and is fully conformant.

## Conformance

The suite lives in `spec/conformance/`, is written in Ruby, and talks to an
implementation only over the endpoints above.

```sh
ORCHESTRATOR_ADAPTER=spec/conformance/adapters/ruby ruby spec/conformance/conformance_test.rb
ORCHESTRATOR_ADAPTER=spec/conformance/adapters/go   ruby spec/conformance/conformance_test.rb
```

It is deliberately not written in Go. A suite sharing a language and a directory
with implementation 1 drifts toward it, and this one has to belong to neither.

An adapter is an executable invoked as

```
adapter --config <path> --repo <dir> --data <dir> --port <n>
```

which runs the orchestrator in the foreground until signalled. Three lines of
shell is a normal size for one.

Each test names the requirements it covers, and
`spec/conformance/coverage_test.rb` fails if any **R**-number in this document
has no test — so a requirement added here without a test is a build failure
rather than an aspiration.

## Implementation Notes

An orchestrator needs to be a single artefact with no runtime dependency on the
app's language, since it sits alongside apps that could be Node, Python or Ruby.
The Go implementation is a static binary for that reason. The Ruby one exists to
keep this document honest and is about 500 lines, which is a fair estimate for
the core.

The Go implementation keeps its orchestrator in `internal/orchestrator`,
depending only on `internal/config` and `internal/proxy`, with no dependency on
its agent — the agent is injected as an `http.Handler` for the paths it serves
and asks for deploys through the CLI like any other caller. Anyone implementing
this spec can leave an agent out entirely, as the Ruby implementation does.
