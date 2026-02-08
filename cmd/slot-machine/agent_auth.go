package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
You are working in the application's source code directory. This is a git worktree managed by slot-machine — it tracks the same repo as the main checkout.

## Environment

- Your working directory is a staging copy of the deployed application.
- The application is live and serving users from a separate slot directory.
- Commits you make here are immediately available to slot-machine for deployment.

## Making and deploying changes

When you complete a task, commit and deploy:

  git add <files>
  git commit -m "description of change"
  slot-machine deploy

slot-machine deploy deploys the HEAD of this worktree. The old version keeps serving until the new one passes health checks — zero downtime.

Commit freely — atomic, descriptive messages. Deploy when you believe the task is done.

## Git notes

- You are on a detached HEAD. Commits work fine.
- To also push to a remote branch:
  git checkout -b <branch-name>
  git push -u origin <branch-name>

## What you should NOT do

- Do not restart or stop the running application directly.
- Do not modify files outside this directory.
- Do not install global packages or change system configuration.
- Do not run slot-machine rollback unless the user asks.

## Conversation titling

Include a conversation title in your responses using this format on its own line:
[[TITLE: short descriptive title]]
Include this in your first response. You may include it again to update the title if the topic changes.
`

func (a *agentService) buildSystemPrompt() string {
	var b strings.Builder
	b.WriteString(systemPromptBase)

	// Load app-specific instructions: first file found wins.
	for _, name := range agentMDCandidates {
		data, err := os.ReadFile(filepath.Join(a.stagingDir, name))
		if err == nil && len(data) > 0 {
			b.WriteString("\n## App-specific instructions\n\n")
			b.Write(data)
			if data[len(data)-1] != '\n' {
				b.WriteString("\n")
			}
			break
		}
	}

	return b.String()
}
