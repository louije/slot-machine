package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Authorization is delegated to the running application.
//
// slot-machine knows who you are — an authenticating proxy told it — but it has
// no idea whether you are allowed. "Allowed" means something different in every
// app: an admin flag, a team membership, a paid plan. The app already holds that
// model, already tests it, and is the only thing that can answer without a
// second roster to keep in sync.
//
// So slot-machine asks. It sends the identity headers it was given to an
// endpoint on the app's INTERNAL_PORT — never the public one — and reads a
// single field out of the reply. It never learns what a role or a group is, and
// there is no mapping table anywhere in this repository.
//
// The contract, in full:
//
//	GET /_slot_machine/access
//	X-Authenticated-User: alice@example.com
//
//	200 {"access": "allAuth"}   any authenticated user; do not ask about individuals
//	200 {"access": "granted"}   this user, yes
//	200 {"access": "denied"}    this user, no
//
// An app that wants no part of this returns "allAuth" in one line and never
// touches a user record.

// accessVerdict is what the app said, reduced to what the daemon acts on.
type accessVerdict struct {
	allow bool
	// blanket is set when the app answered "allAuth" — a statement about the
	// app rather than about the user. Worth keeping distinct from "granted"
	// because it means no decision was made about this person, which is
	// something the UI should be able to say honestly.
	blanket bool
	// reason explains a refusal in operator terms. It reaches the user, so it
	// says what to do, not what went wrong internally.
	reason string
}

// accessTimeout bounds the authorization call.
//
// Short on purpose. This runs in front of every agent request, and an app that
// takes longer than this to answer a question about its own user table is an app
// that is already in trouble — at which point denying is the correct answer
// anyway.
const accessTimeout = 3 * time.Second

// authorize decides whether user may use the agent surface.
//
// The order matters and is the whole security argument:
//
//  1. "none" mode short-circuits. There is no identity, so there is nothing to
//     authorize, and pretending otherwise would mean asking the app about the
//     empty string.
//  2. "allAuth" in config short-circuits. The operator has said, from outside,
//     that authentication is sufficient for this app.
//  3. No live app means no answer, and no answer means no. See the comment on
//     the 503 below — this is the one refusal that is not the user's fault.
//  4. Otherwise the app decides, and anything other than a clean "yes" is a no.
func (a *Service) authorize(r *http.Request, user string) accessVerdict {
	if a.authMode == "none" {
		return accessVerdict{allow: true, blanket: true}
	}
	if a.accessMode == "allAuth" {
		return accessVerdict{allow: true, blanket: true}
	}

	port := 0
	if a.livePort != nil {
		port = a.livePort()
	}
	if port == 0 {
		// Deliberately not a fallback to "allow". The app is the authority on
		// who may use the agent; with no app running there is no authority, and
		// granting the widest access in the least-observed state is how a failed
		// deploy turns into an open door.
		//
		// The recovery path is the API port, which is loopback and
		// unauthenticated: over SSH, `slot-machine status` and `slot-machine
		// rollback` both still work.
		return accessVerdict{reason: "no-live-app"}
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, a.accessEndpoint)
	ctx, cancel := context.WithTimeout(r.Context(), accessTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return accessVerdict{reason: "cannot-ask"}
	}

	// Forward the identity headers verbatim and interpret none of them. This is
	// what lets an app answer from its own database today and from an SSO group
	// claim later without slot-machine changing at all.
	//
	// Only headers whose names begin with the configured prefix set travel: a
	// blanket copy would forward the caller's cookies and Authorization to an
	// endpoint that has no business seeing them.
	req.Header.Set(a.authHeader, user)
	for _, name := range forwardedIdentityHeaders {
		if v := r.Header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}

	resp, err := accessClient.Do(req)
	if err != nil {
		// Timeout, connection refused, app mid-restart. It has an opinion and we
		// cannot read it; guessing is how the wrong person gets in.
		log.Printf("agent: authorization check failed for %q: %v", user, err)
		return accessVerdict{reason: "app-unreachable"}
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		// Distinguished from other failures because it is not a failure: the app
		// simply does not implement the endpoint. It is still a refusal — the
		// operator has to say what they want — but the message can name the fix
		// precisely instead of describing an outage.
		return accessVerdict{reason: "not-implemented"}
	}
	if resp.StatusCode != 200 {
		log.Printf("agent: authorization check for %q returned %d", user, resp.StatusCode)
		return accessVerdict{reason: "app-refused"}
	}

	var body struct {
		Access string `json:"access"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("agent: authorization check for %q returned unreadable JSON: %v", user, err)
		return accessVerdict{reason: "app-unreadable"}
	}

	switch body.Access {
	case "allAuth":
		return accessVerdict{allow: true, blanket: true}
	case "granted":
		return accessVerdict{allow: true}
	case "denied":
		return accessVerdict{reason: "denied"}
	default:
		// An unrecognised value is not a yes. A typo in an app's handler must
		// not open the agent to everyone.
		log.Printf("agent: authorization check for %q returned unknown access %q", user, body.Access)
		return accessVerdict{reason: "app-unreadable"}
	}
}

// forwardedIdentityHeaders are the extra claims an authenticating proxy commonly
// sets alongside the username. They are forwarded so an app can answer from a
// group membership instead of its own user table; slot-machine assigns them no
// meaning whatsoever.
var forwardedIdentityHeaders = []string{
	"X-Auth-Request-Email",
	"X-Auth-Request-Groups",
	"X-Auth-Request-Preferred-Username",
	"X-Forwarded-Email",
	"X-Forwarded-Groups",
}

// accessClient is separate from http.DefaultClient so the timeout below is not
// shared with anything else, and so redirects are refused: an app that redirects
// the authorization check is not answering it.
var accessClient = &http.Client{
	Timeout: accessTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// writeAccessDenial explains a refusal in terms of what the operator must do.
//
// These bodies are the only documentation some people will ever read about this
// mechanism, so they name the endpoint, the config field and the alternative.
func (a *Service) writeAccessDenial(w http.ResponseWriter, v accessVerdict, user string) {
	switch v.reason {
	case "no-live-app":
		http.Error(w, "slot-machine has no live application, so it cannot determine who is "+
			"allowed to use the agent. Deploy a version, or set \"agent_access\": \"allAuth\" "+
			"in slot-machine.json to allow every authenticated user.", 503)

	case "app-unreachable", "app-refused", "app-unreadable":
		http.Error(w, "the application could not answer whether you may use the agent. "+
			"This is not a decision about you: slot-machine refuses when it cannot ask. "+
			"Check the application's logs.", 503)

	case "not-implemented":
		http.Error(w, fmt.Sprintf("the application does not implement %s, so slot-machine "+
			"cannot tell who may use the agent. Implement it to return "+
			"{\"access\": \"granted\"} or {\"access\": \"denied\"}, or set "+
			"\"agent_access\": \"allAuth\" in slot-machine.json to allow every "+
			"authenticated user.", a.accessEndpoint), 503)

	default:
		// A real decision about a real person: 403, not 503, and no detail about
		// why beyond the fact that the app said so.
		log.Printf("agent: %q is authenticated but not authorized", user)
		http.Error(w, "the application does not permit you to use the agent.", 403)
	}
}
