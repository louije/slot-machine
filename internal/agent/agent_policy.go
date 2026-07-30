package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Agent tool policy.
//
// What this is and is not, stated plainly because the distinction has been
// blurred before in this codebase:
//
// The agent runs as the same user as the daemon, with a shell, and with the
// app's environment including its secrets. There is no sandbox. These rules stop
// an agent from *casually* doing something catastrophic — they do not stop an
// agent that is determined to route around them, and nothing written in JSON
// can. The real boundary, if one is ever needed, is a separate uid or a
// container; see docs/design.md §11.
//
// Two things we know about how the rules behave, both learned the hard way:
//
//   - Matching is a prefix match against the command *as typed*. A rule written
//     against an expanded path (/home/you/.ssh/*) never matches `cat ~/.ssh/id`,
//     because the agent typed a tilde. Rules must be written the way a command
//     is actually written.
//   - An allow list cannot tighten anything. Adding entries only ever widens.
//     Restricting requires `deny`.
//
// The policy file lives in the data directory, not in the agent's worktree.
// Claude Code refuses agent writes to .claude/settings*.json, so the file tools
// could not edit it there either — but a shell in that directory could delete
// it, and worse, it was an untracked file inside the app's repository that
// `git add -A` would happily commit, complete with absolute server paths.
// Outside the worktree, neither is possible by accident.

// deniedCommands are shapes that are never a legitimate step in editing and
// deploying a web app, written as they would actually be typed.
//
// Kept deliberately short. A long list of near-misses reads as protection
// without being any, and every entry that fires on legitimate turn is an entry
// that teaches the agent to find another way around.
var deniedCommands = []string{
	// Destroying the machine or someone else's data.
	"rm -rf /",
	"rm -rf /*",
	"rm -fr /",
	"rm -rf ~",
	"rm -rf $HOME",
	"mkfs",
	"dd if=",

	// Host and service management: the daemon's job, never the agent's.
	"sudo",
	"doas",
	"su ",
	"systemctl",
	"service ",
	"launchctl",
	"shutdown",
	"reboot",
	"halt",
	"apt",
	"apt-get",
	"yum",
	"dnf",
	"pacman",
	"brew install",
	"brew uninstall",

	// Rewriting shared history, which no deploy needs and no human can undo
	// from the other end.
	"git push --force",
	"git push -f",
	"git filter-branch",
	"git reflog delete",

	// The daemon owns process and slot lifecycle. An agent that runs these is
	// fighting the thing that supervises it.
	"slot-machine start",
	"slot-machine init",
	"slot-machine install",
	"slot-machine update",
}

// agentPolicyPath is where the generated settings file lives.
func (a *Service) agentPolicyPath() string {
	return filepath.Join(a.dataDir, "agent-settings.json")
}

// writeAgentPolicy regenerates the policy file before every turn, so an agent
// that removed it gets it back on its next message.
func (a *Service) writeAgentPolicy() error {
	absConfig, err := filepath.Abs(a.configPath)
	if err != nil {
		absConfig = a.configPath
	}

	deny := []string{
		// The config is the orchestrator's, not the app's: it defines the
		// integration points, so an agent that can edit it can redefine what
		// "healthy" means.
		"Edit(" + absConfig + ")",
		"Write(" + absConfig + ")",
		// The daemon's own data directory, including this file and the agent
		// binary it manages.
		"Edit(" + a.dataDir + "/**)",
		"Write(" + a.dataDir + "/**)",
		"Read(" + filepath.Join(a.dataDir, "agent.db") + ")",
	}

	// Deploy keys and anything else under ~/.ssh. The file tools honour these;
	// a shell does not, which is why the README says so rather than implying
	// otherwise.
	if home, err := os.UserHomeDir(); err == nil {
		sshDir := filepath.Join(home, ".ssh")
		deny = append(deny,
			"Read("+sshDir+"/**)",
			"Edit("+sshDir+"/**)",
			"Write("+sshDir+"/**)",
			"Read(~/.ssh/**)",
			"Edit(~/.ssh/**)",
			"Write(~/.ssh/**)",
		)
	}

	for _, cmd := range deniedCommands {
		deny = append(deny, "Bash("+cmd+":*)")
	}
	for _, cmd := range a.deniedCommands {
		deny = append(deny, "Bash("+cmd+":*)")
	}

	settings := map[string]any{
		"permissions": map[string]any{"deny": deny},
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.agentPolicyPath(), append(data, '\n'), 0644)
}
