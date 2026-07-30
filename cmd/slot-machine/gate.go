package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The pre-promotion gate — docs/design.md §5, "Continuous Validation".
//
// Everything here answers one question: is it safe to make this change live?
// The checks run against the diff between what is currently serving traffic and
// the candidate commit, which is exactly the change production is about to
// experience.
//
// Scope and honesty about it: the agent runs as the same uid as the daemon,
// with a shell, and the daemon API is on localhost. There is therefore no
// override flag, because any override reachable by the operator's CLI is
// equally reachable by the agent — an override here would be decoration, not a
// control. The gate is a guardrail against a confused agent, not a boundary
// against an adversarial one. A human who needs to bypass it edits
// slot-machine.json and restarts, which is deliberate and out of band.

// builtinSecretPatterns are deliberately specific. A generic high-entropy rule
// catches real secrets and also minified assets, lockfile hashes and UUIDs —
// and since there is no override, a false positive blocks the operator's own
// deploys too. Precision matters more than recall here.
var builtinSecretPatterns = []string{
	`-----BEGIN [A-Z ]*PRIVATE KEY-----`,
	`AKIA[0-9A-Z]{16}`,
	`gh[pousr]_[A-Za-z0-9]{36}`,
	`sk-ant-[A-Za-z0-9_\-]{20,}`,
	`xox[baprs]-[A-Za-z0-9-]{10,}`,
	`glpat-[A-Za-z0-9_\-]{20}`,
}

func compileSecretPatterns(extra []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(builtinSecretPatterns)+len(extra))
	for _, p := range builtinSecretPatterns {
		out = append(out, regexp.MustCompile(p))
	}
	for _, p := range extra {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("secret_patterns: %q is not a valid regexp: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// gateError is a refusal to promote, carrying enough detail for the agent to
// act on it without guessing.
type gateError struct {
	Check  string
	Detail string
}

func (e *gateError) Error() string { return e.Check + ": " + e.Detail }

// runGate performs the static checks against diff(base..candidate).
//
// base is the live commit. On a first deploy there is no live commit and
// nothing about production is being changed, so the diff-based checks are
// skipped — there is no meaningful "change" to measure.
func (o *orchestrator) runGate(base, candidate string) error {
	if base == "" {
		return nil
	}
	if base == candidate {
		return nil
	}

	rng := base + ".." + candidate

	changed, err := git(o.repoDir, "diff", "--name-only", rng)
	if err != nil {
		return &gateError{Check: "diff", Detail: err.Error()}
	}
	files := splitLines(changed)

	if err := o.checkProtectedPaths(files); err != nil {
		return err
	}
	if err := o.checkDiffSize(rng); err != nil {
		return err
	}
	if err := o.checkSecrets(rng); err != nil {
		return err
	}
	return o.checkSilentRevert(candidate)
}

func (o *orchestrator) checkProtectedPaths(files []string) error {
	for _, f := range files {
		for _, p := range o.cfg.ProtectedPaths {
			if pathUnder(f, p) {
				return &gateError{
					Check:  "protected path",
					Detail: fmt.Sprintf("%s is protected by protected_paths (%q) and this deploy modifies it", f, p),
				}
			}
		}
	}
	return nil
}

// pathUnder reports whether file is p itself or lives beneath it. Compared
// segment-wise so that "docs" does not match "docsite/x".
func pathUnder(file, p string) bool {
	file = filepath.Clean(file)
	p = filepath.Clean(strings.TrimSuffix(p, "/"))
	if file == p {
		return true
	}
	return strings.HasPrefix(file, p+"/")
}

func (o *orchestrator) checkDiffSize(rng string) error {
	if o.cfg.MaxDiffLines <= 0 {
		return nil
	}
	out, err := git(o.repoDir, "diff", "--numstat", rng)
	if err != nil {
		return &gateError{Check: "diff size", Detail: err.Error()}
	}
	total := 0
	for _, line := range splitLines(out) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// "-" means a binary file; it contributes no line count.
		add, _ := strconv.Atoi(fields[0])
		del, _ := strconv.Atoi(fields[1])
		total += add + del
	}
	if total > o.cfg.MaxDiffLines {
		return &gateError{
			Check:  "diff size",
			Detail: fmt.Sprintf("%d changed lines exceeds max_diff_lines (%d)", total, o.cfg.MaxDiffLines),
		}
	}
	return nil
}

// checkSecrets scans only added lines. Pre-existing content is not this
// deploy's doing, and flagging it would make the gate unpassable.
func (o *orchestrator) checkSecrets(rng string) error {
	patterns, err := compileSecretPatterns(o.cfg.SecretPatterns)
	if err != nil {
		return &gateError{Check: "secret scan", Detail: err.Error()}
	}

	out, err := git(o.repoDir, "diff", "--unified=0", rng)
	if err != nil {
		return &gateError{Check: "secret scan", Detail: err.Error()}
	}

	file := ""
	for _, line := range splitLines(out) {
		if strings.HasPrefix(line, "+++ b/") {
			file = strings.TrimPrefix(line, "+++ b/")
			continue
		}
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		for _, re := range patterns {
			if re.MatchString(line) {
				return &gateError{
					Check: "secret scan",
					Detail: fmt.Sprintf(
						"an added line in %s matches %s — remove the credential, or add a narrower rule to secret_patterns if this is a false positive",
						file, re.String()),
				}
			}
		}
	}
	return nil
}

// checkSilentRevert is the one divergence rule that blocks rather than warns.
//
// If the candidate's tree is missing files that the human branch has, promoting
// it removes that work from production with no conflict and no error — the
// failure is invisible, which is what makes it worth stopping. Ordinary
// divergence (behind by N commits) is reported in /status and left alone: the
// agent is the one that merges, and it needs to be able to deploy while it
// catches up.
func (o *orchestrator) checkSilentRevert(candidate string) error {
	humanRef := o.humanRef()
	if humanRef == "" {
		return nil
	}

	out, err := git(o.repoDir, "diff", "--name-only", "--diff-filter=A", candidate, humanRef)
	if err != nil {
		// A missing ref is not a gate failure; it means we cannot judge.
		logf("gate: cannot compare against %s: %v", humanRef, err)
		return nil
	}
	missing := splitLines(out)
	if len(missing) == 0 {
		return nil
	}

	shown := missing
	if len(shown) > 10 {
		shown = shown[:10]
	}
	return &gateError{
		Check: "silent revert",
		Detail: fmt.Sprintf(
			"this commit is missing %d file(s) that %s has, so deploying it would remove them from production without any conflict: %s. Merge %s first",
			len(missing), humanRef, strings.Join(shown, ", "), humanRef),
	}
}

// humanRef prefers the remote-tracking branch, since that is what "what the
// humans have" means once a remote exists. Falls back to the local branch, and
// returns "" when neither is present — on a fresh install with no remote there
// is nothing to compare against.
func (o *orchestrator) humanRef() string {
	remote := "origin/" + o.cfg.HumanBranch
	if gitOK(o.repoDir, "rev-parse", "--verify", "--quiet", remote) {
		return remote
	}
	if gitOK(o.repoDir, "rev-parse", "--verify", "--quiet", o.cfg.HumanBranch) {
		return o.cfg.HumanBranch
	}
	return ""
}

// branchDivergence reports how far the machine branch is from the human branch.
// Purely informational — it feeds /status so that both the operator and the
// agent can see drift before it becomes a surprise.
type branchDivergence struct {
	Ref    string `json:"ref,omitempty"`
	Ahead  int    `json:"ahead"`
	Behind int    `json:"behind"`
}

func (o *orchestrator) machineDivergence() *branchDivergence {
	humanRef := o.humanRef()
	if humanRef == "" {
		return nil
	}
	if !gitOK(o.repoDir, "rev-parse", "--verify", "--quiet", o.cfg.MachineBranch) {
		return nil
	}
	out, err := git(o.repoDir, "rev-list", "--left-right", "--count",
		humanRef+"..."+o.cfg.MachineBranch)
	if err != nil {
		return nil
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return nil
	}
	behind, _ := strconv.Atoi(fields[0])
	ahead, _ := strconv.Atoi(fields[1])
	return &branchDivergence{Ref: humanRef, Ahead: ahead, Behind: behind}
}

func splitLines(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
