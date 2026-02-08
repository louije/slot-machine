# Agent & Chat

*Extends [design.md](design.md) and [orchestrator-spec.md](orchestrator-spec.md)*

slot-machine gains agent capabilities: an embedded Claude Code backend and a
chat UI, served through the existing reverse proxy. No new daemons, no new
ports, no infrastructure changes. The app adds one HTML tag.

## How it fits

The orchestrator already manages processes, routes traffic, and survives
deploys. The agent service reuses all of that:

```
Browser → port 3000 (slot-machine reverse proxy)
  ├─ /chat             → slot-machine serves the chat UI
  ├─ /agent/*          → slot-machine handles agent API
  └─ everything else   → forwarded to the live slot's app process
```

From the browser's perspective, the chat is part of the app. Same origin,
same domain, same port. No CORS, no exposed internal ports, no proxy config.

slot-machine's reverse proxy is already an HTTP-level proxy
(`httputil.ReverseProxy`). It inspects the path before forwarding — requests
to `/chat` and `/agent/*` are handled internally, everything else goes to
the app. The deploy/rollback/status API stays on the separate API port
(default 9100), which is localhost-only.

## Embedding the chat

### Iframe (one line, zero customization)

```html
<iframe src="/chat" style="width:100%;height:600px;border:none;"></iframe>
```

Self-contained. The iframe loads a full page from slot-machine. It has its
own styles, its own JS, its own SSE connections. The host app doesn't need to
know anything about it.

### JS module (full control)

```html
<script src="/chat.js"></script>
<div id="agent"></div>
<script>SlotMachine.chat('#agent')</script>
```

The module renders into the given container. The host app controls layout and
can override styles. API calls go to `/agent/*` on the same origin.

Both modes work because the chat is served through the app's own port via
the proxy intercept. No cross-origin issues.

## Agent sessions

A session is a Claude Code CLI process managed by slot-machine. The lifecycle:

```
1. User sends a message           POST /agent/conversations/:id/messages
2. slot-machine spawns claude     claude --output-format stream-json
                                         --resume <session_id>
                                         --cwd <staging_slot>
                                         -p "message"
3. Stream output to browser       GET /agent/conversations/:id/stream (SSE)
4. Claude runs tools, edits code  (in the staging slot directory)
5. Process exits                  session_id saved for resume
6. User sends another message     → step 2, with --resume
```

### Process management

slot-machine already manages app processes (spawn, health check, drain,
SIGKILL on timeout). Agent processes use the same primitives:

- **Spawn**: `exec.Command("claude", ...)` with stdout pipe for stream-json
  parsing. The process runs in the staging slot's directory with the app's
  env vars loaded.
- **Monitor**: if the process hangs (no output for N minutes), kill it. If
  the process exits unexpectedly, report the error via SSE.
- **Cancel**: SIGTERM, wait 5 seconds, SIGKILL. The user can cancel from the
  chat UI.
- **Concurrency**: one active session at a time (same as deploy locking).
  A second request while the agent is running gets rejected or queued.

### Session resumption

Claude Code sessions persist across messages via `--resume <session_id>`.
The session ID is captured from the CLI's stream-json output (the `system`
event with `subtype: "init"`) and stored in the conversation record.

When the user sends a follow-up message, the CLI resumes with the previous
session — no need to replay history, and the agent retains its internal
context (files read, plans made, tool results).

If resume fails (corrupted session, tool_use ID conflict), the fallback is
automatic: retry without `--resume`, including conversation history in the
prompt instead.

## Conversations

Conversations are stored in a SQLite database in the data directory
(`<data>/agent.db`). Schema:

```
conversations
  id            TEXT PRIMARY KEY
  title         TEXT
  session_id    TEXT          -- Claude session for --resume
  created_at    DATETIME
  updated_at    DATETIME

messages
  id            INTEGER PRIMARY KEY
  conversation_id TEXT
  type          TEXT          -- user, assistant, tool_use, tool_result, system, error
  content       TEXT          -- text or JSON
  created_at    DATETIME
```

Messages are stored **before** being sent via SSE. If the browser
disconnects mid-stream, the conversation is intact. The client reconnects
and reads the full conversation from the API.

This is the lesson from Matometa: database-first, SSE second. The SSE stream
is a nice-to-have on top of a reliable storage layer.

## Agent API

All endpoints are served through the reverse proxy on the app's public port.

```
POST   /agent/conversations              → create conversation
GET    /agent/conversations              → list conversations
GET    /agent/conversations/:id          → get conversation with messages
POST   /agent/conversations/:id/messages → send message (starts agent)
GET    /agent/conversations/:id/stream   → SSE stream of agent output
POST   /agent/conversations/:id/cancel   → cancel running agent
DELETE /agent/conversations/:id          → delete conversation
```

### SSE events

```
event: assistant
data: {"content": "I'll fix the footer layout."}

event: tool_use
data: {"tool": "Edit", "input": {"file_path": "client/styles.css", ...}}

event: tool_result
data: {"tool": "Edit", "output": "File edited successfully"}

event: system
data: {"content": "init", "session_id": "abc-123"}

event: error
data: {"content": "Process exited with code 1"}

event: done
data: {"conversation_id": "...", "usage": {"input_tokens": 1234, ...}}
```

The message format is normalized from Claude's stream-json output. The chat
UI doesn't need to know whether the backend is CLI or SDK — the events are
the same.

## Chat UI

A console, not a messaging app. Served at `/chat` — vanilla JS, no
framework, no build step. Self-contained HTML + CSS + JS.

```
┌─────────────────────────────────────────────────────┐
│  ┌─ conversations ──────────────┐                    │
│  │ fixing the footer layout   ● │  [×]               │
│  │ update the colors            │                    │
│  │ deploy v2.3                  │                    │
│  └──────────────────────────────┘                    │
│                                                      │
│  You: the footer overlaps the content on mobile      │
│                                                      │
│  Agent: I see the issue — the footer has position:   │
│  fixed but no bottom padding on the body.            │
│                                                      │
│  ┌──────────────────────────────────────────────┐    │
│  │ ▸ Edit client/styles.css                     │    │
│  │ ▸ Read client/index.html                     │    │
│  └──────────────────────────────────────────────┘    │
│                                                      │
│  Fixed. Want me to deploy?                           │
│                                                      │
│  ┌──────────────────────────────────────────────┐    │
│  │ message…                                 [⏎] │    │
│  └──────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────┘
```

The primary view is a single conversation. Past conversations are accessible
via a dropdown or popover at the top — not a persistent sidebar. The focus is
the current interaction.

- **Streaming text**: characters appear as they arrive via SSE
- **Tool visibility**: collapsed pills showing what the agent is reading,
  editing, running. Expandable for details.
- **Markdown rendering**: code blocks, tables, lists in agent responses
- **Deploy status**: when the agent triggers a deploy, progress shows inline
- **Cancel button**: stops the running agent process

## Deploy-through

The central lifecycle problem from [design.md §8](design.md):

> The app in slot A spawns the agent. The agent writes to slot B, calls
> deploy, the orchestrator swaps traffic to slot B, drains and stops slot A
> — killing the agent.

This doesn't happen here because the agent is a child of slot-machine, not
the app. When slot-machine drains an app process, it doesn't touch its own
children. The Claude CLI process keeps running.

The full sequence when the agent deploys:

```
1. Agent edits code in staging slot
2. Agent commits to machine branch
3. Agent calls POST /deploy (function call within slot-machine)
4. slot-machine promotes staging → live
   - Old live → prev (drained, stopped)
   - Proxy switches to new slot's port
   - New staging created (CoW clone)
5. Chat UI stays connected (SSE is on the proxy, which didn't restart)
6. Agent reports "Deployed" via SSE
7. Agent continues working in the new staging slot if needed
```

Step 5 is the key: the SSE connection is between the browser and
slot-machine's proxy. The proxy didn't restart. The agent process didn't
restart. Only the app process swapped. The chat session is uninterrupted.

If the agent wants to keep working after deploy (next task, follow-up edit),
it operates in the new staging slot. The `--resume` session preserves
Claude's memory, but the filesystem context has shifted to the fresh clone.
The agent's CLAUDE.md should note this.

## Agent privileges

Three layers constrain what the agent can do.

### File tools — scoped to the staging slot

`Read`, `Write`, `Edit`, `Glob`, `Grep` operate relative to the staging
slot directory via `--cwd`. The agent can freely read and write code in its
workspace but can't accidentally modify the live slot, slot-machine's own
files, or system paths through these tools.

### Bash — allowlisted commands

The agent needs to *run things*, not just edit files. The default allowlist:

```
Bash(git:*)              # commit, branch, merge, pull
Bash(sqlite3:*)          # query the app's database
Bash(python:*)           # run data scripts, analysis
Bash(bun:*)              # run app commands, tests
Bash(node:*)             # same
Bash(curl:*)             # call APIs, check endpoints
Bash(slot-machine:*)     # deploy, rollback, status
```

This blocks raw shell (`rm`, `kill`, `systemctl`, `apt`) while allowing the
agent to do real work — query the database, run tests, and deploy.

The gap: `Bash(python:*)` can technically `import os; os.system(...)`. This
isn't a security boundary against an adversary. It's a guardrail against an
agent that occasionally gets confused. Claude won't intentionally escape the
sandbox. The restrictions prevent "I'll clean up by running `rm -rf`"
moments, which is the actual risk. Real sandboxing (containers) is a later
concern if ever needed.

### Skills — slot-machine integration

The agent deploys by running `slot-machine deploy` — same CLI a human would
use. No special internal API, no skills to maintain. The system prompt tells
the agent how:

```
To deploy:   git add -A && git commit -m "..." && slot-machine deploy
To rollback: slot-machine rollback
To check:    slot-machine status
```

## Agent prompt

slot-machine injects a system prompt when spawning the Claude CLI. It
combines two layers:

1. **slot-machine context** — the deployment workflow, branch model, and
   constraints. Same for all apps. Injected via `--system-prompt`.
2. **App context** — what the app does, how it works, project-specific
   instructions. Read from a file in the repo (`CLAUDE.md` or the path in
   `agent_prompt`). Loaded by Claude Code automatically when `--cwd` points
   to a repo that contains it.

The slot-machine context covers:

### Working directory

> You are working in the staging slot of a slot-machine deployment. This
> directory is a copy of the app's repository. Your edits here do not affect
> the live app until you deploy.

### Two kinds of work

> **Feature work** — edit code, commit, deploy. Use file tools (Read, Edit,
> Write) for code changes in the staging slot. When you're done, commit and
> deploy.
>
> **Operations** — query the database, inspect state, run data scripts,
> analyze logs. Use Bash commands. The database is at `$DB_PATH` (or whatever
> the app uses). State files and logs are in shared locations outside the
> slots — you're reading live data, not a staging copy.
>
> Not every task ends in a deploy. If the user asks you to query something,
> fix data, or investigate an issue, do it and report back.

### Branch model

> The repo has two branches:
>
> - `main` — the human branch. Humans push here. You do not commit to main.
> - `machine` — your branch. You commit and push here.
>
> Before starting work, pull and merge the latest main:
>
>     git fetch origin main
>     git merge origin/main -m "merge main"
>
> If the merge conflicts, resolve it — you understand the codebase and can
> make the right call. If you're unsure, ask the user.
>
> After your work is deployed and validated, humans accept your changes by
> merging `machine` into `main` via pull request.

### Deploy workflow

> To make your changes live:
>
>     git add -A
>     git commit -m "concise description of the change"
>     slot-machine deploy
>
> This promotes the staging slot to live. The old live becomes the rollback
> target. A new staging slot is created as a copy of what you just deployed.
>
> If the deploy fails (health check doesn't pass), the live app is
> untouched. Investigate, fix, and try again.
>
> To check the current state: `slot-machine status`
> To roll back to the previous version: `slot-machine rollback`

### After a deploy

> After a successful deploy, the staging slot is recreated as a fresh copy
> of the promoted slot. If you need to keep working, you are now in this
> new staging directory. Your session memory persists but the filesystem
> has shifted to the fresh copy. Previous file handles or paths may be
> stale — re-read any file you need.

### Constraints

> - Do not modify `.git/config` or change remotes
> - Do not run `slot-machine start` or `slot-machine init` — the daemon is
>   already running
> - Do not kill processes or modify system configuration
> - The app's database is available at the path in `$DB_PATH` (or whatever
>   the app uses). You can query it — be careful with writes

The app's own `CLAUDE.md` adds project-specific context: what the app does,
how the code is structured, database schema, API endpoints, coding
conventions, anything the agent needs to work effectively.

## App environment access

The agent runs with the app's environment — same env vars, same filesystem
access. This is intentional. The agent's value comes from understanding the
app's runtime context:

- Reading the database to understand data shapes
- Running app-specific commands (migrations, seeds, data fixes)
- Checking logs, state files, caches
- Testing changes against real data

When spawning the Claude CLI, slot-machine:

1. Sets `cwd` to the staging slot directory
2. Loads the app's `env_file` into the process environment
3. Sets `SLOT_MACHINE=1`, `PORT`, `INTERNAL_PORT` (same as app processes)
4. Injects the slot-machine system prompt (branch model, deploy commands,
   constraints)

The agent has the same access as someone who `cd`s into the staging slot and
starts poking around. That's the point — it's a developer with the app's
full context, not a sandboxed tool operating blind.

## Configuration

No new config fields are required. The agent uses what's already in
`slot-machine.json`:

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

The agent inherits `env_file` for the app's environment and uses the data
directory for its SQLite database. The staging slot path comes from the
orchestrator's own state.

Optional agent-specific fields (with sensible defaults):

| Field | Default | What it does |
|-------|---------|-------------|
| `agent_prompt` | `"CLAUDE.md"` | System prompt file, relative to repo root |
| `agent_timeout_s` | `600` | Max seconds before a stuck session is killed |
| `agent_allowed_tools` | (see above) | Bash command allowlist; `null` = all tools |

If these fields aren't in the config, the agent works anyway. Zero-config by
default, tunable when needed.

## Security

### Network

The chat UI and agent API are served through the app's public port via the
reverse proxy intercept. They're protected by whatever protects the app —
Tailscale, Caddy with auth, HTTP basic auth, nothing (local dev).

The deploy/rollback/status API stays on the separate API port (9100), which
is localhost-only. The agent calls deploy/rollback/status internally, never
exposing them through the public port.

### Auth on agent routes

The proxy intercept can optionally require authentication on `/chat` and
`/agent/*` paths. Options, in order of complexity:

1. **None** — appropriate behind Tailscale or for local dev
2. **Shared secret** — a token in a cookie or header, checked by slot-machine
3. **Delegate to app** — slot-machine calls the app's auth endpoint to
   validate the request before handling it

For v1, option 1. The system is designed for a single operator on a private
network.
