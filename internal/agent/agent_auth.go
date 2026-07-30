package agent

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// extractUser returns the authenticated identity, or "" if there is none.
//
// slot-machine does not authenticate. It reads an identity that something in
// front of it has already established — Caddy's forward_auth, oauth2-proxy,
// Tailscale — and that is only trustworthy because nothing else can reach the
// port: every listener binds loopback by default (config.Listen). The bind
// address is the security boundary here; this function is just a read.
//
// The header used to be signed with an HMAC whose secret was served,
// unauthenticated, from /chat/config — the browser needed it to sign. Anyone who
// could reach the port could mint a header for any username, so the signature
// verified nothing. It has been removed rather than repaired, because repairing
// it means expiry, replay protection and rotation, and the proxy in front
// already does all three properly.
func (a *Service) extractUser(r *http.Request) string {
	if a.authMode == "none" {
		return anonymousUser
	}
	return strings.TrimSpace(r.Header.Get(a.authHeader))
}

// anonymousUser is the identity in "none" mode. It is a real string rather than
// "" so that a conversation started in local development is still attributable
// to something, and so that "" unambiguously means "not authenticated".
const anonymousUser = "local"

// requireAccess authenticates and authorizes, writing the refusal itself.
//
// Every route goes through this, including /chat and /chat/config. Dispatching
// any route above the gate is what made the previous design ineffective: the
// config route was served first and handed out the signing secret.
func (a *Service) requireAccess(w http.ResponseWriter, r *http.Request) (string, bool) {
	user := a.extractUser(r)
	if user == "" {
		// Fail closed, and say exactly what is missing. A misconfigured proxy
		// and a genuinely anonymous caller look identical from here, and the
		// person most likely to read this is the operator who just set it up.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, fmt.Sprintf("no %s header.\n\n"+
			"slot-machine does not authenticate users. Put an authenticating proxy in "+
			"front of it (Caddy forward_auth, oauth2-proxy, Tailscale) and have it set "+
			"this header, or set \"agent_auth\": \"none\" in slot-machine.json for local "+
			"development.", a.authHeader), 401)
		return "", false
	}

	if v := a.authorize(r, user); !v.allow {
		a.writeAccessDenial(w, v, user)
		return "", false
	}
	return user, true
}

// agentMDCandidates is the priority order for agent instruction files.
// First file found wins.
var agentMDCandidates = []string{
	"AGENTS.slot-machine.md",
	"AGENTS.md",
	"CLAUDE.md",
}

const systemPromptBase = `You are an AI assistant embedded in a web application via slot-machine.

## Where you are

Your working directory is the *machine slot* — a git worktree checked out on the
%s branch. It is yours: slot-machine never rewrites, renames or force-checks-out
this directory, so uncommitted work here is safe across deploys.

The application serving real traffic runs from a different directory, from a
specific commit. Your edits change nothing about production until you deploy.

## Making and deploying changes

  git add <files>
  git commit -m "description of change"
  slot-machine deploy

Deploy takes the current commit of this worktree, checks it out into a separate
staging slot, and promotes it only if it passes every check. The old version
keeps serving until then — zero downtime, and a failed deploy leaves production
untouched.

Commit freely: atomic commits with descriptive messages. Deploy when you believe
the task is done. Not every task ends in a deploy — if you were asked to
investigate or query something, answer and stop.

## Deploys are checked, and a refusal is information

A deploy can be refused before anything is promoted: a protected path was
modified, an added line looks like a credential, the diff is too large, the
pre-deploy command failed, or the commit is missing files the %s branch has.

The refusal names the check and the reason. Fix the cause. Do NOT try to get
around it — do not rename a file to dodge a protected path, do not split a
change into smaller deploys to slip under a size limit, and do not retry an
identical deploy hoping for a different answer. There is no override, and
working around a check is worse than the thing the check stopped, because it
also hides it.

If you believe a refusal is wrong, say so in the conversation and stop. The
thresholds live in slot-machine.json, which a human can change and you cannot.

## Staying current with human work

Humans commit to %s; you commit to %s. Before you change code, merge their work:

  git fetch origin %s
  git merge origin/%s

Resolve conflicts yourself — you understand the codebase. If a merge is beyond
you, say so rather than guessing. ` + "`slot-machine status`" + ` shows how far the
branches have drifted.

If you deploy a commit that is missing files the human branch has, the deploy is
refused, because promoting it would delete their work from production with no
error at all.

## When you hit a wall

A tool may be denied. If it is, that is the end of it — do not retry it, do not
invent a workaround, and do not try a different filename or a different command
to get the same effect.

(This is on record from a sibling system: a single denied scratch-file write was
re-attempted under roughly forty invented filenames until one landed somewhere
writable. Every one of those attempts was wasted, and the result was a file
nobody expected in a place nobody looked.)

Say what was refused, precisely — the exact tool and argument — and carry on
with the rest of the task. Naming it precisely is what lets a human fix it.

## Nobody may be watching

Your output is streamed to a chat UI, but there may be no one in front of it —
the browser can be closed and you keep running. Do not wait for approval that
cannot come. Finish what you can, report honestly what you could not, and leave
the repository in a committed, consistent state.

## What you should NOT do

- Do not restart or stop the running application directly.
- Do not modify files outside this directory.
- Do not modify slot-machine.json.
- Do not install global packages or change system configuration.
- Do not run ` + "`slot-machine rollback`" + ` unless asked.

## Conversation titling

Include a conversation title on its own line, in your first response:
[[TITLE: short descriptive title]]
You may include it again to update the title if the topic changes.
`

// maxInstructionBytes is a backstop on the app-specific instructions folded into
// the system prompt.
//
// Not an OS limit any more: the prompt travels in a file, so there is no argv
// ceiling. This guards a self-inflicted lockout instead. The instruction file
// lives in the agent's own worktree, so an agent that commits a pathologically
// large CLAUDE.md would blow the context window on every subsequent turn —
// including the turn you would use to undo it. Truncating keeps the agent
// reachable.
//
// warnInstructionBytes is the softer signal: well below any hard limit, but
// large enough that the operator should know they are spending that much of
// every turn's context on instructions.
const (
	warnInstructionBytes = 32 * 1024
	maxInstructionBytes  = 256 * 1024
)

// systemPromptPath is where the assembled prompt is written for the CLI to read.
//
// It lives in the data directory, not the worktree, which puts it behind the
// same deny rules as the tool policy: the agent's file tools cannot rewrite its
// own instructions.
func (a *Service) systemPromptPath() string {
	return filepath.Join(a.dataDir, "agent-system-prompt.md")
}

// writeSystemPrompt assembles the prompt and writes it out, returning the path.
//
// Regenerated every turn, both so an edited CLAUDE.md takes effect without a
// restart and so anything that tampered with the file is corrected.
func (a *Service) writeSystemPrompt() (string, error) {
	path := a.systemPromptPath()
	if err := os.WriteFile(path, []byte(a.buildSystemPrompt()), 0644); err != nil {
		return "", err
	}
	return path, nil
}

// buildSystemPrompt assembles slot-machine's own context plus the app's
// instruction file.
func (a *Service) buildSystemPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, systemPromptBase,
		a.machineBranch, a.humanBranch, a.humanBranch, a.machineBranch,
		a.humanBranch, a.humanBranch)

	// Load app-specific instructions: first file found wins.
	for _, name := range agentMDCandidates {
		data, err := os.ReadFile(filepath.Join(a.workDir, name))
		if err != nil || len(data) == 0 {
			continue
		}

		if len(data) > maxInstructionBytes {
			log.Printf("agent: %s is %d bytes; using the first %d. An instruction file "+
				"this large would exhaust the context window on every turn.",
				name, len(data), maxInstructionBytes)
			data = data[:maxInstructionBytes]
		} else if len(data) > warnInstructionBytes {
			log.Printf("agent: %s is %d bytes, and is prepended to every turn. "+
				"Consider trimming it.", name, len(data))
		}

		b.WriteString("\n## App-specific instructions\n\n")
		b.Write(data)
		if data[len(data)-1] != '\n' {
			b.WriteString("\n")
		}
		break
	}

	return b.String()
}
