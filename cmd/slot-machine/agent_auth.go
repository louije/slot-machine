package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func (a *agentService) extractUser(r *http.Request) string {
	header := r.Header.Get("X-SlotMachine-User")
	switch a.authMode {
	case "hmac":
		idx := strings.LastIndex(header, ":")
		if idx < 1 {
			return ""
		}
		user, sig := header[:idx], header[idx+1:]
		mac := hmac.New(sha256.New, []byte(a.authSecret))
		mac.Write([]byte(user))
		expected := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(sig), []byte(expected)) {
			return ""
		}
		return user
	case "trusted":
		return header
	default:
		return ""
	}
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

// maxInstructionBytes caps the app-specific instructions folded into the system
// prompt.
//
// The prompt is passed as a single command-line argument, and Linux limits one
// argument to MAX_ARG_STRLEN — 32 pages, 128 KiB — regardless of how much total
// argv space is available. An app whose CLAUDE.md grew past that would not get a
// truncated prompt, it would get E2BIG and no agent at all. 64 KiB leaves ample
// room for the base prompt and is far larger than any instruction file that is
// still useful to an agent.
const maxInstructionBytes = 64 * 1024

// buildSystemPrompt assembles slot-machine's own context plus the app's
// instruction file.
//
// Note for operators: this ends up in the agent process's argv, so it is visible
// in `ps` to any local user. That is not a new exposure — the agent already
// receives the app's environment, secrets included — but it is worth knowing
// before putting anything in CLAUDE.md that you would not put in a process
// listing.
func (a *agentService) buildSystemPrompt() string {
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
			logf("agent: %s is %d bytes; using the first %d and ignoring the rest "+
				"(the system prompt is one command-line argument and cannot exceed the OS limit)",
				name, len(data), maxInstructionBytes)
			data = data[:maxInstructionBytes]
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
