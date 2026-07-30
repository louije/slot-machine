package agent

import "regexp"

// Failure classification for spawned agent runs.
//
// Three outcomes matter and they are not the same thing:
//
//	transient — the API wobbled (429/5xx/overloaded). Retrying works.
//	terminal  — the account is out of budget or the session lost auth. Retrying
//	            is guaranteed to fail; only a human can clear it, so say so
//	            plainly in the chat instead of surfacing an exit code.
//	unknown   — everything else. Reported, not retried.
//
// Ported from meduse's process manager, including the reason it exists: between
// 2026-06-20 and 2026-07-05, 385 of its scheduled runs died on
// `API Error: 401 Invalid authentication credentials`, and on 2026-07-21/22
// another 18 died on the monthly spend limit — 16 hours of unbroken monitoring
// loss. Every one failed silently, because the only classifier was the transient
// regex and a non-match meant "give up quietly". The patterns below come from
// those runs' verbatim output.
//
// slot-machine had the same hole in a worse form: it discarded the agent's
// stderr entirely, so an expired CLAUDE_CODE_OAUTH_TOKEN surfaced in the chat as
// "Agent exited with error: exit status 1" and nothing else.

// transientAPIRe matches API failures worth retrying.
var transientAPIRe = regexp.MustCompile(
	`API Error: (429|5\d\d)|overloaded_error|rate_limit_error|api_error`)

// terminalAPIRe matches failures where retrying cannot help.
//
// Deliberately not a catch-all for 4xx: a 400 is a bug in how we built the
// invocation, and telling the user "a human must clear this" would be wrong.
// These four patterns mean the agent is locked out until someone acts.
var terminalAPIRe = regexp.MustCompile(
	`monthly spend limit|hit your session limit|API Error: 401|Invalid authentication credentials`)

type failureKind int

const (
	failureUnknown failureKind = iota
	failureTransient
	failureTerminal
)

// classifyFailure inspects a failed run's last assistant text plus its stderr
// tail.
//
// Terminal is checked first: a spend-limit message can arrive alongside
// unrelated rate-limit chatter in the same buffer, and "out of money" outranks
// "try again" — retrying a capped account produces three failures instead of one.
func classifyFailure(text string) failureKind {
	switch {
	case terminalAPIRe.MatchString(text):
		return failureTerminal
	case transientAPIRe.MatchString(text):
		return failureTransient
	default:
		return failureUnknown
	}
}

// terminalReason turns a terminal failure into something worth showing a human.
func terminalReason(text string) string {
	switch {
	case regexp.MustCompile(`monthly spend limit`).MatchString(text):
		return "the account's monthly spend limit has been reached"
	case regexp.MustCompile(`hit your session limit`).MatchString(text):
		return "the account's session limit has been reached"
	default:
		return "Claude API authentication failed (401) — check CLAUDE_CODE_OAUTH_TOKEN"
	}
}
