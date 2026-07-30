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
slot-machine update        # update to latest GitHub release
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

If the live app crashes, the public port keeps answering — with `503`, not a
refused connection. The distinction matters: a refused connection looks the same
as a machine that has gone away, and letting go of the port invites something
else to take it while the app is down.

## What a deploy checks

Every deploy — from the agent or from your terminal — runs the same pipeline,
and stops at the first thing that fails:

```
resolve   commit must exist                    → else 400
gate      protected paths, secrets, diff size,
          and "would this delete work that main has?"
prepare   check the commit out into staging
setup     setup_command
verify    pre_deploy_command (your test suite)  → non-zero blocks promotion
boot      start the process on a fresh port
probe     health_endpoint, then schema_status
promote   switch the proxy, drain the old process
```

The gate runs **before** `setup_command`, because `bun install` and `npm ci`
execute code from the commit being deployed — a secret scan that runs after that
has already lost. `verify` runs after, because a test suite needs its
dependencies.

A refused deploy names the stage and the reason, and the live slot is untouched:

```
$ slot-machine deploy
deploy failed at gate: secret scan: an added line in deploy.sh matches
gh[pousr]_[A-Za-z0-9]{36} — remove the credential, or add a narrower rule to
secret_patterns if this is a false positive
```

There is deliberately **no override flag**. The agent runs as the same user as
the daemon, with a shell, and the API is on localhost — so any override reachable
from your terminal is equally reachable by the agent, and would be decoration
rather than a control. The gate is a guardrail against a confused agent, not a
boundary against an adversarial one. To bypass it, change `slot-machine.json`
and restart: deliberate, and out of band.

For that to happen, the daemon manages four **slots** — git worktrees of the app:

- **live** — serving traffic through the reverse proxy
- **prev** — the previous deploy, ready for instant rollback
- **staging** — where a deploy is prepared and health-checked before promotion
- **machine** — the agent's own worktree, checked out on the `machine` branch

The machine slot belongs to the agent. slot-machine never renames it, never
force-checks-it-out and never garbage-collects it, so dependencies and
uncommitted work survive deploys and daemon restarts. Deploys happen entirely in
the staging slot, which means the agent can keep working while its own change is
being promoted.

```
  ┌──────────────────────────────────────────────────────────────┐
  │                    slot-machine daemon                       │
  │                                                              │
  │  ┌──────────────┐ ┌──────────────┐ ┌───────────┐ ┌────────┐ │
  │  │ slot-a3f2... │ │ slot-7c36... │ │slot-staging│ │machine │ │
  │  │ (prev)       │ │ (live)       │ │(deploys)  │ │(agent) │ │
  │  │              │ │              │ │           │ │        │ │
  │  │ rollback     │ │ :51234 app   │ │ gate +    │ │ branch │ │
  │  │ target       │ │ :51235 int   │ │ health    │ │ machine│ │
  │  └──────────────┘ └──────┬───────┘ └───────────┘ └────────┘ │
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
| `shared_dirs` | `[]` | Directories symlinked across deploys (e.g. `["data", "uploads"]`). Must be gitignored — a tracked path fights the checkout, and slot-machine warns at startup if it finds one |
| `agent_auth` | `hmac` | Agent auth mode (see below) |
| `agent_allowed_tools` | Bash, Edit, Read, Write, Glob, Grep | Claude tools the agent can use |
| `agent_model` | CLI default | `claude --model`. Unset means the run inherits the server user's `~/.claude/settings.json` |
| `agent_timeout_s` | `1800` | Max seconds for one agent turn before it is stopped |
| `machine_branch` | `machine` | Branch the agent commits to |
| `human_branch` | `main` | Branch you commit to |
| `protected_paths` | `[]` | Paths a deploy may not modify |
| `secret_patterns` | `[]` | Extra regexes, added to the built-in credential patterns |
| `max_diff_lines` | `0` (off) | Reject a deploy whose diff is larger than this |
| `pre_deploy_command` | — | Run in staging after setup; non-zero blocks promotion |
| `pre_deploy_timeout_ms` | `120000` | How long `pre_deploy_command` may take |
| `schema_status_endpoint` | — | Path serving schema compatibility (see Migrations) |
| `chat_title` | `slot-machine` | Title shown in the chat header |
| `chat_accent` | `#2563eb` | CSS accent color for the chat UI |

Missing values get the defaults above. A config that fails validation stops the
daemon with the reason rather than starting in a broken state.

### Auth modes

| Mode | When to use |
|------|------------|
| `hmac` | Default. HMAC-SHA256 signatures, secret generated per daemon session. |
| `trusted` | Behind a reverse proxy that handles auth upstream (e.g. Caddy + basic auth). Username passed in header, no verification. |
| `none` | Local development only. No auth. |

### Authentication (Claude API)

The agent needs an OAuth token from `claude login`. Set it in the app environment:

```sh
# systemd
Environment=CLAUDE_CODE_OAUTH_TOKEN=your-token-here

# or in your env_file (.env)
CLAUDE_CODE_OAUTH_TOKEN=your-token-here
```

### Agent tools, and what they are not

Default tools: `Bash`, `Edit`, `Read`, `Write`, `Glob`, `Grep`. Add more in
`slot-machine.json`:

```json
{
  "agent_allowed_tools": ["Bash", "Edit", "Read", "Write", "Glob", "Grep", "WebSearch", "WebFetch"]
}
```

**There is no sandbox.** The agent runs as the same user as the daemon, with a
shell, and with the app's environment including its secrets. Read that sentence
before deciding where to run this.

What slot-machine does provide is a deny list, written to
`.slot-machine/agent-settings.json` before every turn and passed to the CLI. It
covers the shapes that are never a legitimate step in editing and deploying an
app — `sudo`, `systemctl`, package managers, `rm -rf /`, `git push --force`,
`shutdown`, and the `slot-machine` subcommands that fight the daemon — plus the
config file, the data directory, and `~/.ssh`. Add your own:

```json
{
  "agent_denied_commands": ["terraform", "kubectl", "psql"]
}
```

Two limits worth understanding, because both have bitten people:

- **Matching is a prefix match against the command as typed.** A rule written
  against an expanded path never matches the tilde form the agent actually
  types. Write rules the way a command gets written.
- **An allow list only ever widens.** Restricting requires `deny`.

So this is a guardrail against a confused agent, not a boundary against a
determined one — `python -c`, `sed`, and a dozen other allowed tools each grant
arbitrary effects, and no JSON file changes that. If you need a real boundary,
run the agent under its own uid or in a container; see `docs/design.md` §11.

The policy file deliberately lives in the data directory rather than in the
agent's worktree. In the worktree it was an untracked file that `git add -A`
would commit into your app's repository, absolute server paths and all.

### Claude binary

slot-machine installs the Claude Code CLI automatically on first start into
`.slot-machine/.local/bin/claude`, by piping the official installer to a shell.
If you would rather that not happen on your server, install it yourself and
point slot-machine at it:

```sh
export SLOT_MACHINE_AGENT_BIN=/path/to/claude
```

### Updating slot-machine

`slot-machine update` verifies the downloaded binary against the `checksums.txt`
published with the release, and refuses to install anything if that file is
missing or the hash does not match. A tool that supervises deploys should not
replace itself with unverified bytes.

### Deploy key (git push from agent)

To let the agent push to a remote branch, add a deploy key:

1. Generate a key: `ssh-keygen -t ed25519 -C "deploy" -f deploy-key -N ""`
2. Add the public key to GitHub (repo → Settings → Deploy keys, enable write access)
3. Put the private key on the server at `~/.ssh/<name>` with `chmod 600`
4. Configure `~/.ssh/config`:
   ```
   Host github.com
     IdentityFile ~/.ssh/<name>
     IdentitiesOnly yes
   ```
5. Add the remote to the bare repo: `git remote add origin git@github.com:user/repo.git`

slot-machine denies the agent's `Read`/`Edit`/`Write` tools on `~/.ssh/**`, so
the file tools cannot reach the key. **A shell can.** See below.

### Branch model

You commit to `main`. The agent commits to `machine`, in its own worktree. It
merges your work before changing code:

```sh
git fetch origin main && git merge origin/main
```

That merge is the agent's job — it understands the codebase and can resolve
conflicts contextually. slot-machine's job is to *notice* when it hasn't
happened. `slot-machine status` reports how far the branches have drifted, and
one case is refused outright: deploying a commit whose tree is missing files
that `main` has. That would delete your work from production with no conflict
and no error, which is the only divergence failure that is completely silent.

Ordinary drift is reported, not blocked — the agent has to be able to deploy
while it catches up.

### Migrations

Code deploys are reversible in seconds; migrations are not. slot-machine's
involvement is purely observational: set `schema_status_endpoint` and the app
serves

```json
{
  "current_schema_version": 42,
  "code_min_schema_version": 41,
  "code_max_schema_version": 43,
  "pending_migrations": [],
  "compatible": true
}
```

The orchestrator reads it after the health check — on deploys *and* on
rollbacks — and refuses to promote code that cannot read the current schema.
It never connects to the database, never parses SQL, and never runs a
migration. The dangerous case it catches is rolling back to code that predates
a migration which has already run.

Leave `schema_status_endpoint` unset and the check is skipped entirely.

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
| `POST` | `/deploy` | `{"commit":"abc..."}` → deploy. On failure returns `stage` and `error` |
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
| `DELETE` | `/agent/conversations/:id` | Delete a conversation (409 while its agent is working) |
| `POST` | `/agent/conversations/:id/messages` | Send message |
| `GET` | `/agent/conversations/:id/stream` | SSE stream (`system`, `assistant`, `tool_use`, `tool_result`, `done`, `status`) |
| `POST` | `/agent/conversations/:id/cancel` | Kill running agent |

## Code layout

```
internal/config        the app contract: load, defaults, validation
internal/proxy         the reverse proxy that owns the public port
internal/orchestrator  slots, processes, health, the gate, rollback
internal/agent         the chat service and the agent supervisor
internal/agent/store   conversation and event persistence
cmd/slot-machine       the CLI, and the wiring between the above
```

`orchestrator` and `agent` do not import each other, and that is the point. The
agent asks for a deploy by running `slot-machine deploy`, the same command a
human runs, so there is no privileged internal path to secure. The orchestrator
takes the agent as an `http.Handler` and knows nothing else about it.

## Tests

```sh
go test ./...
```

Black-box spec tests in `spec/` cover the full contract: deploy, rollback,
health checks, crash detection, drain timeout, concurrent deploy rejection,
zero-downtime switching, symlink persistence, GC, daemon restart recovery,
agent streaming, the pre-promotion gate, the machine slot, and CLI behavior.
Unit tests in `cmd/slot-machine/`.

CI runs `gofmt`, `go vet` and the full suite on every push.

## TODO

- [ ] Reconciliation loop: poll the remote for pushes the webhook missed
- [ ] Alert webhook on consecutive failed deploys
- [ ] Periodic `git gc` and disk-pressure checks

## License

MIT
