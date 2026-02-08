# slot-machine

Add a chat agent to a web app that can discuss state and update and commit code live through
slotted zero-downtime deploys.

```
slot-machine init          # detect project, generate config
slot-machine start         # start daemon, manage deploys
slot-machine deploy        # deploy HEAD (or a specific commit)
slot-machine rollback      # swap back to previous slot
slot-machine status        # what's running
slot-machine install       # copy binary to ~/.local/bin
```

## How it works

The chat UI is served at `/chat` on the app port. Behind it, Claude Code runs
in the staging worktree with access to the source code, a shell, and the
`slot-machine deploy` command.

**Two things it can do:**

- **Explore state** — "why is this page slow?", "what does the payment
  flow look like?", "show me recent errors in the logs". The agent reads
  source files, greps through code, runs commands, and inspects databases or
  logs in `shared_dirs` to answer questions about the running application.

- **Change code** — "add a dark mode toggle", "fix the broken link on the
  about page", "update the footer copyright year". The agent edits files,
  commits, and runs `slot-machine deploy`. The new version boots and passes
  health checks while the old one keeps serving — if anything goes wrong, the
  live slot is untouched.

Deploys are zero-downtime: the new process starts and passes health checks
before traffic switches. The old process drains gracefully. If the new process
fails health checks, it's killed and the live slot stays untouched. Rollback
is always one command away.

For that to happen, the daemon manages three **slots** — git worktrees of the app:

- **live** — serving traffic through the reverse proxy
- **prev** — the previous deploy, ready for instant rollback
- **staging** — a workspace where the chat agent reads, edits, and deploys code

```
  ┌──────────────────────────────────────────────────────────────┐
  │                    slot-machine daemon                       │
  │                                                              │
  │   ┌───────────────┐  ┌───────────────┐  ┌───────────────┐    │
  │   │  slot-a3f2... │  │  slot-7c36... │  │  slot-staging │    │
  │   │  (prev)       │  │  (live)       │  │  (workspace)  │    │
  │   │               │  │               │  │               │    │
  │   │  rollback     │  │  :51234 app   │  │  chat agent   │    │
  │   │  target       │  │  :51235 int   │  │  works here   │    │
  │   └───────────────┘  └───────┬───────┘  └───────────────┘    │
  │                              │                               │
  │                     reverse proxy                            │
  │                     :3000 ──►┘                               │
  │                     :3000/chat ──► agent UI                  │
  └──────────────────────────────────────────────────────────────┘
```

## Getting started

slot-machine takes a git repository with a web app that listens on a port and
exposes a health endpoint. Node/Bun, Python/uv, and Ruby/Bundler projects are
detected out of the box.

### 1. Install

```sh
go install slot-machine/cmd/slot-machine@latest
# or: go build -o slot-machine ./cmd/slot-machine/ && ./slot-machine install
```

### 2. Initialize

```sh
cd your-app
slot-machine init
```

This detects the project type (`bun.lock` → Bun, `package-lock.json` → npm,
`uv.lock` → uv, `Gemfile.lock` → Bundler) and generates `slot-machine.json`:

```json
{
  "start_command": "bun server/index.ts",
  "setup_command": "bun install --frozen-lockfile",
  "port": 3000,
  "health_endpoint": "/healthz",
  "health_timeout_ms": 10000,
  "drain_timeout_ms": 5000,
  "env_file": ".env"
}
```

The app must:
- Listen on the `PORT` environment variable (slot-machine assigns dynamic ports)
- Return 200 on the `health_endpoint` path

### 3. Start

```sh
slot-machine start
```

The daemon starts, creates the three slots, auto-deploys HEAD, and begins
proxying traffic. The chat agent is available at `http://localhost:3000/chat`.

### 4. Teach the agent about the app

An `AGENTS.md` (or `AGENTS.slot-machine.md` or `CLAUDE.md`) in the repo root
gets injected into the agent's system prompt — stack details, conventions,
what not to touch:

```markdown
_Example_.  
This is a Bun + HTMX app. SQLite database in data/app.db.

- Run tests with `bun test`
- Never modify data/*.db directly, use the ORM
- CSS is in public/styles.css, no build step
```

### 5. Deploy and rollback

From the CLI, or from the chat:

```sh
slot-machine deploy          # deploy current HEAD
slot-machine deploy abc123   # deploy a specific commit
slot-machine rollback        # swap back to previous slot
slot-machine status          # check what's live
```

## Configuration

All fields in `slot-machine.json`:

| Field | Default | What it does |
|-------|---------|-------------|
| `start_command` | — | How to start the app |
| `setup_command` | — | Runs after checkout, before start (e.g. install deps) |
| `port` | — | Public port — daemon reverse-proxies this to the live slot |
| `internal_port` | same as `port` | Separate health check port, if the app uses one |
| `health_endpoint` | — | Path to poll for 200 OK |
| `health_timeout_ms` | `10000` | How long to wait for healthy before giving up |
| `drain_timeout_ms` | `5000` | Graceful shutdown window before SIGKILL |
| `env_file` | — | Loaded into the app's environment |
| `api_port` | `9100` | Daemon API port (deploy/rollback/status) |
| `shared_dirs` | `[]` | Directories symlinked across deploys (e.g. `["data", "uploads"]`) |
| `agent_auth` | `hmac` | Agent auth mode (see below) |
| `agent_allowed_tools` | Bash, Edit, Read, Write, Glob, Grep | Claude tools the agent can use |
| `chat_title` | `slot-machine` | Title shown in the chat header |
| `chat_accent` | `#2563eb` | CSS accent color for the chat UI |

### Auth modes

| Mode | When to use |
|------|------------|
| `hmac` | Default. HMAC-SHA256 signatures, secret generated per daemon session. |
| `trusted` | Behind a reverse proxy that handles auth upstream (e.g. Caddy + basic auth). Username passed in header, no verification. |
| `none` | Local development only. No auth. |

### Custom styling

A `chat.css` in the project root overrides CSS variables:

```css
:root {
  --sm-accent: #e11d48;
  --sm-bg: #fafafa;
}
```

### Environment variables

slot-machine injects these into the app process:

| Variable | Value |
|----------|-------|
| `PORT` | Dynamic port for the app to listen on |
| `INTERNAL_PORT` | Dynamic port for health checks (if `internal_port` differs from `port`) |
| `SLOT_MACHINE` | Always `1` — detect that the app is running under slot-machine |

## API

### Daemon API (`:9100` by default)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | Health check |
| `POST` | `/deploy` | `{"commit":"abc..."}` → deploy |
| `POST` | `/rollback` | Swap to previous slot |
| `GET` | `/status` | Current state |

### Chat API (app port, intercepted by proxy)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/chat` | Chat UI |
| `GET` | `/chat/config` | Auth and display config |
| `GET` | `/chat.css` | Custom CSS from project root |
| `GET` | `/agent/conversations` | List conversations |
| `POST` | `/agent/conversations` | Create conversation |
| `GET` | `/agent/conversations/:id` | Conversation with messages |
| `POST` | `/agent/conversations/:id/messages` | Send message |
| `GET` | `/agent/conversations/:id/stream` | SSE stream (`system`, `assistant`, `tool_use`, `tool_result`, `done`) |
| `POST` | `/agent/conversations/:id/cancel` | Kill running agent |

## Tests

```sh
go test ./...
```

Black-box spec tests in `spec/` cover the full contract: deploy, rollback,
health checks, crash detection, drain timeout, concurrent deploy rejection,
zero-downtime switching, symlink persistence, GC, daemon restart recovery,
agent streaming, and CLI behavior. Unit tests in `cmd/slot-machine/`.

## TODO

- [ ] Implement migration policy

## License

MIT
