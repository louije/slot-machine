// Agent service and proxy intercept spec tests.
//
// These validate two architectural claims from agent.md:
//
//  1. Proxy intercept: the reverse proxy handles /agent/* and /chat paths
//     internally (slot-machine serves them), forwarding everything else to
//     the app. Same origin, same port, no CORS.
//
//  2. Deploy-through: the agent process is a child of slot-machine, not the
//     app. When the app is swapped during a deploy, the agent keeps running
//     and the SSE connection through the proxy stays connected.
package spec

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test 26: Proxy forwards app traffic
// ---------------------------------------------------------------------------
//
// After deploying an app, requests to non-intercepted paths (like /)
// should be forwarded to the app through the reverse proxy.

func TestProxyForwardsAppTraffic(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Deploy an app.
	mustDeploy(t, apiPort, repo.CommitA)

	// GET / through the proxy should be forwarded to the app.
	code, body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", appPort))
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}

	// The testapp returns {"status":"ok","port":...} — verify we got the app's response.
	if !strings.Contains(body, `"status"`) {
		t.Fatalf("expected app response with status field, got: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Test 27: Proxy intercepts /agent/* paths
// ---------------------------------------------------------------------------
//
// Requests to /agent/* should be handled by slot-machine, not forwarded
// to the app. The test verifies by checking that the response is NOT the
// testapp's usual {"status":"ok","port":...} response.

func TestProxyInterceptsAgentPaths(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Deploy so the proxy is active.
	mustDeploy(t, apiPort, repo.CommitA)

	// GET /agent/conversations through the proxy.
	code, body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/agent/conversations", appPort))

	if code != 200 {
		t.Fatalf("expected 200 for /agent/conversations, got %d", code)
	}

	// Must NOT be the app's response (testapp returns {"status":"ok","port":...} for all paths).
	if strings.Contains(body, `"port"`) {
		t.Fatal("/agent/conversations was forwarded to the app — expected slot-machine to intercept it")
	}

	// Should be a JSON response (empty list of conversations).
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, "[") && !strings.HasPrefix(body, "{") {
		t.Fatalf("expected JSON response from /agent/conversations, got: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Test 28: Proxy intercepts /chat path
// ---------------------------------------------------------------------------
//
// The /chat path serves the chat UI — an HTML page, not the app's JSON.

func TestProxyInterceptsChatPath(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	// Deploy so the proxy is active.
	mustDeploy(t, apiPort, repo.CommitA)

	// GET /chat through the proxy.
	code, body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/chat", appPort))

	if code != 200 {
		t.Fatalf("expected 200 for /chat, got %d", code)
	}

	// Must NOT be the app's JSON response.
	if strings.Contains(body, `"port"`) {
		t.Fatal("/chat was forwarded to the app — expected slot-machine to intercept it")
	}

	// Should contain HTML.
	if !strings.Contains(body, "<") {
		t.Fatalf("expected HTML response for /chat, got: %s", body)
	}
}

// ---------------------------------------------------------------------------
// Test 29: Agent process survives app deploy (deploy-through)
// ---------------------------------------------------------------------------
//
// The central lifecycle claim: the agent is a child of slot-machine, not the
// app. When a deploy swaps app processes, the agent keeps running and the
// SSE connection through the proxy stays connected.
//
// Sequence:
//   1. Deploy app A
//   2. Start an agent session (spawns testagent via the agent API)
//   3. Open SSE stream, verify events are flowing
//   4. Deploy app B while agent is running
//   5. Verify SSE stream still receives events after the deploy
//
// The testagent binary outputs stream-json events at 1-second intervals
// for 10 seconds, giving enough time for a deploy to complete mid-stream.

func TestAgentSurvivesDeploy(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)
	agentBin := testagentBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	// The agent must still be running when the deploy lands, and a deploy under
	// load can take longer than a fixed number of agent events. Give the agent a
	// lifetime that outlasts any plausible deploy so this tests process survival
	// rather than which of the two finishes first.
	orch := startOrchestratorWithAgentEnv(t, bin, contract, repo.Dir, apiPort, agentBin, release,
		[]string{"TESTAGENT_DURATION=200", "TESTAGENT_INTERVAL=200"})
	_ = orch

	// 1. Deploy app A.
	dr := mustDeploy(t, apiPort, repo.CommitA)

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", appPort)

	// 2. Create a conversation.
	resp, err := http.Post(proxyURL+"/agent/conversations", "application/json", nil)
	if err != nil {
		t.Fatalf("creating conversation: %v", err)
	}
	var conv struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&conv)
	resp.Body.Close()
	if conv.ID == "" {
		t.Fatal("expected conversation ID in response")
	}

	// 3. Send a message — this starts the testagent process.
	msgBody, _ := json.Marshal(map[string]string{"content": "test deploy-through"})
	resp, err = http.Post(
		fmt.Sprintf("%s/agent/conversations/%s/messages", proxyURL, conv.ID),
		"application/json",
		bytes.NewReader(msgBody),
	)
	if err != nil {
		t.Fatalf("sending message: %v", err)
	}
	resp.Body.Close()

	// 4. Open SSE stream.
	sseClient := &http.Client{Timeout: 0} // no timeout for streaming
	sseResp, err := sseClient.Get(fmt.Sprintf("%s/agent/conversations/%s/stream", proxyURL, conv.ID))
	if err != nil {
		t.Fatalf("opening SSE stream: %v", err)
	}
	defer sseResp.Body.Close()

	// Read events in background.
	type sseEvent struct {
		eventType string
		data      string
	}
	events := make(chan sseEvent, 100)
	go func() {
		scanner := bufio.NewScanner(sseResp.Body)
		var currentEvent string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event:") {
				currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				events <- sseEvent{
					eventType: currentEvent,
					data:      strings.TrimSpace(strings.TrimPrefix(line, "data:")),
				}
			}
		}
		close(events)
	}()

	// 5. Wait for at least one assistant event.
	deadline := time.After(10 * time.Second)
	gotEventBeforeDeploy := false
	for !gotEventBeforeDeploy {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("SSE stream closed before deploy")
			}
			if ev.eventType == "assistant" {
				gotEventBeforeDeploy = true
			}
		case <-deadline:
			t.Fatal("no assistant SSE events received before deploy")
		}
	}

	// 6. Deploy app B while agent is running.
	dr, _ = deploy(t, apiPort, repo.CommitB)
	if !dr.Success {
		t.Fatal("deploy B failed while agent was running")
	}

	// 7. Verify SSE stream still receives events after deploy.
	deadline = time.After(15 * time.Second)
	gotEventAfterDeploy := false
	for !gotEventAfterDeploy {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("SSE stream closed after deploy — agent process was killed")
			}
			if ev.eventType == "assistant" || ev.eventType == "done" {
				gotEventAfterDeploy = true
			}
		case <-deadline:
			t.Fatal("no SSE events received after deploy — agent may have been killed")
		}
	}
}

// ---------------------------------------------------------------------------
// Test 30: Auto-titling extracts [[TITLE:...]] from agent output
// ---------------------------------------------------------------------------
//
// The testagent emits [[TITLE: <prompt>]] in its first assistant message.
// The orchestrator should:
//   1. Strip the [[TITLE:...]] from the SSE stream data
//   2. Store the extracted title in the database (visible via GET conversation)

func TestAutoTitling(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)
	agentBin := testagentBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestratorWithAgent(t, bin, contract, repo.Dir, apiPort, agentBin, release)
	_ = orch

	// Deploy so the agent service is active.
	mustDeploy(t, apiPort, repo.CommitA)

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", appPort)

	// Create a conversation.
	resp, err := http.Post(proxyURL+"/agent/conversations", "application/json", nil)
	if err != nil {
		t.Fatalf("creating conversation: %v", err)
	}
	var conv struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&conv)
	resp.Body.Close()
	if conv.ID == "" {
		t.Fatal("expected conversation ID")
	}

	// Send a message to start the agent.
	msgBody, _ := json.Marshal(map[string]string{"content": "fix the login bug"})
	resp, err = http.Post(
		fmt.Sprintf("%s/agent/conversations/%s/messages", proxyURL, conv.ID),
		"application/json",
		bytes.NewReader(msgBody),
	)
	if err != nil {
		t.Fatalf("sending message: %v", err)
	}
	resp.Body.Close()

	// Open SSE stream and collect events.
	sseClient := &http.Client{Timeout: 0}
	sseResp, err := sseClient.Get(fmt.Sprintf("%s/agent/conversations/%s/stream", proxyURL, conv.ID))
	if err != nil {
		t.Fatalf("opening SSE stream: %v", err)
	}
	defer sseResp.Body.Close()

	// Read all SSE data lines until stream closes or timeout.
	type sseEvent struct {
		eventType string
		data      string
	}
	events := make(chan sseEvent, 100)
	go func() {
		scanner := bufio.NewScanner(sseResp.Body)
		var currentEvent string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event:") {
				currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				events <- sseEvent{
					eventType: currentEvent,
					data:      strings.TrimSpace(strings.TrimPrefix(line, "data:")),
				}
			}
		}
		close(events)
	}()

	// Collect all assistant data lines. Verify none contain [[TITLE:...]].
	deadline := time.After(15 * time.Second)
	var assistantDataLines []string
	done := false
	for !done {
		select {
		case ev, ok := <-events:
			if !ok {
				done = true
				break
			}
			if ev.eventType == "assistant" {
				assistantDataLines = append(assistantDataLines, ev.data)
			}
			if ev.eventType == "done" {
				done = true
			}
		case <-deadline:
			t.Fatal("timeout waiting for SSE events")
		}
	}

	// None of the SSE data lines should contain [[TITLE:...]].
	for _, line := range assistantDataLines {
		if strings.Contains(line, "[[TITLE:") {
			t.Fatalf("SSE stream should not contain [[TITLE:...]] marker, got: %s", line)
		}
	}

	// GET the conversation — the title should have been extracted and stored.
	code, body := httpGet(t, fmt.Sprintf("%s/agent/conversations/%s", proxyURL, conv.ID))
	if code != 200 {
		t.Fatalf("expected 200 for GET conversation, got %d", code)
	}

	// Parse the response to find the title.
	var convResp struct {
		Conversation struct {
			Title string `json:"title"`
		} `json:"conversation"`
	}
	if err := json.Unmarshal([]byte(body), &convResp); err != nil {
		t.Fatalf("parsing conversation response: %v", err)
	}
	if convResp.Conversation.Title != "fix the login bug" {
		t.Fatalf("expected title %q, got %q", "fix the login bug", convResp.Conversation.Title)
	}
}

// ---------------------------------------------------------------------------
// Test 31: authentication is read, authorization is delegated
// ---------------------------------------------------------------------------
//
// End to end, through the real proxy, with a real app answering. The unit tests
// cover the branches; this covers the wiring — that the header survives the
// reverse proxy, that the daemon finds the live slot's internal port, and that
// the app's verdict reaches the caller.
//
// The previous version of this test asserted that /chat returned 200 without
// any header, "not auth-protected". That was the leak, written down as a
// requirement: /chat loads /chat/config, and /chat/config served the HMAC
// signing secret to whoever asked.

func TestAgentAuthReadsHeaderAndDelegatesAccess(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContractWithAuth(t, t.TempDir(), appPort, intPort, 0, "header")

	startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	mustDeploy(t, apiPort, repo.CommitA)

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", appPort)

	// Every agent path, with no identity at all. /chat is in this list on
	// purpose: it is the page that fetches the config, and there is no version
	// of "the UI is public but the API is not" that is coherent.
	t.Run("no identity is refused everywhere", func(t *testing.T) {
		for _, path := range []string{"/chat", "/chat.css", "/chat/config", "/agent/conversations"} {
			code, body := httpGet(t, proxyURL+path)
			if code != 401 {
				t.Errorf("GET %s: got %d, want 401\n%s", path, code, body)
			}
		}
		if code := httpPost(t, proxyURL+"/agent/conversations"); code != 401 {
			t.Errorf("POST /agent/conversations: got %d, want 401", code)
		}
	})

	// The testapp grants anyone whose name starts with "admin".
	t.Run("the app grants", func(t *testing.T) {
		code, body := httpGetAs(t, proxyURL+"/chat", "admin@example.com")
		if code != 200 {
			t.Fatalf("GET /chat as admin: got %d, want 200\n%s", code, body)
		}
		if !strings.Contains(body, "<html") {
			t.Fatalf("expected the chat page, got: %s", body)
		}
		if code, _ := httpGetAs(t, proxyURL+"/agent/conversations", "admin@example.com"); code != 200 {
			t.Errorf("GET /agent/conversations as admin: got %d, want 200", code)
		}
	})

	// Authenticated, and refused by the app. 403 rather than 401: the identity
	// was accepted, the permission was not.
	t.Run("the app denies", func(t *testing.T) {
		code, body := httpGetAs(t, proxyURL+"/chat", "intern@example.com")
		if code != 403 {
			t.Fatalf("GET /chat as intern: got %d, want 403\n%s", code, body)
		}
	})

	// And the secret is gone for good: even a fully authorized caller cannot
	// obtain a credential from the config route, because there is none.
	t.Run("no credential is served to anyone", func(t *testing.T) {
		code, body := httpGetAs(t, proxyURL+"/chat/config", "admin@example.com")
		if code != 200 {
			t.Fatalf("GET /chat/config as admin: got %d, want 200", code)
		}
		if strings.Contains(body, "authSecret") || strings.Contains(body, "Secret") {
			t.Fatalf("/chat/config is serving a credential again: %s", body)
		}
	})
}

// ---------------------------------------------------------------------------
// Test 31b: with nothing live, there is nobody to ask
// ---------------------------------------------------------------------------
//
// The user's decision, made explicitly: no app, no chat. The alternative —
// granting when no app is live — would mean a failed deploy widens access, in
// the state nobody is watching.
//
// The daemon starts here with no successful deploy at all, which is also what a
// fresh install looks like.

func TestAgentRefusesWhenNoAppIsLive(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)

	// A start command that never becomes healthy, so the daemon's auto-deploy of
	// HEAD fails and no slot is ever live. This is the state a fresh box is in
	// before its first successful deploy, and the state a broken app leaves
	// behind — the two cases where "ask the app" has nobody to ask.
	contract := writeContract(t, t.TempDir(), map[string]any{
		"start_command":     "sleep 60",
		"port":              appPort,
		"internal_port":     intPort,
		"health_endpoint":   "/healthz",
		"health_timeout_ms": 1500,
		"drain_timeout_ms":  500,
		"agent_auth":        "header",
	})

	startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)

	// The proxy is bound even so — that is the point of binding it at startup —
	// and the status API confirms nothing is live before anything is asserted.
	if commit := status(t, apiPort).LiveCommit; commit != "" {
		t.Fatalf("expected no live slot, got %s", commit)
	}

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", appPort)
	code, body := httpGetAs(t, proxyURL+"/chat", "admin@example.com")
	if code != 503 {
		t.Fatalf("GET /chat with nothing live: got %d, want 503\n%s", code, body)
	}
	// 503 and not 403, because nothing was decided about this person — and the
	// body has to say what to do, since the chat they would use to ask is the
	// thing that is refusing.
	if !strings.Contains(body, "agent_access") {
		t.Errorf("the refusal should name the config field that changes it, got: %s", body)
	}

	// The recovery path the whole design leans on: the API port is loopback and
	// unauthenticated, so an operator on the box is never locked out of the
	// controls even when the agent surface is refusing everyone.
	status(t, apiPort)
}

// ---------------------------------------------------------------------------
// Test 32: Tool events forwarded through SSE
// ---------------------------------------------------------------------------
//
// The testagent emits tool_use and tool_result events. The orchestrator should
// forward them as SSE events with the correct types and data structure.

func TestToolEventsForwardedThroughSSE(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)
	agentBin := testagentBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestratorWithAgent(t, bin, contract, repo.Dir, apiPort, agentBin, release)
	_ = orch

	mustDeploy(t, apiPort, repo.CommitA)

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", appPort)

	// Create conversation and send message.
	resp, err := http.Post(proxyURL+"/agent/conversations", "application/json", nil)
	if err != nil {
		t.Fatalf("creating conversation: %v", err)
	}
	var conv struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&conv)
	resp.Body.Close()

	msgBody, _ := json.Marshal(map[string]string{"content": "test tool events"})
	resp, err = http.Post(
		fmt.Sprintf("%s/agent/conversations/%s/messages", proxyURL, conv.ID),
		"application/json",
		bytes.NewReader(msgBody),
	)
	if err != nil {
		t.Fatalf("sending message: %v", err)
	}
	resp.Body.Close()

	// Open SSE stream.
	sseClient := &http.Client{Timeout: 0}
	sseResp, err := sseClient.Get(fmt.Sprintf("%s/agent/conversations/%s/stream", proxyURL, conv.ID))
	if err != nil {
		t.Fatalf("opening SSE stream: %v", err)
	}
	defer sseResp.Body.Close()

	type sseEvent struct {
		eventType string
		data      string
	}
	events := make(chan sseEvent, 100)
	go func() {
		scanner := bufio.NewScanner(sseResp.Body)
		var currentEvent string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event:") {
				currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				events <- sseEvent{
					eventType: currentEvent,
					data:      strings.TrimSpace(strings.TrimPrefix(line, "data:")),
				}
			}
		}
		close(events)
	}()

	// Collect events until done or timeout.
	deadline := time.After(20 * time.Second)
	var toolUseEvents, toolResultEvents []string
	done := false
	for !done {
		select {
		case ev, ok := <-events:
			if !ok {
				done = true
				break
			}
			switch ev.eventType {
			case "tool_use":
				toolUseEvents = append(toolUseEvents, ev.data)
			case "tool_result":
				toolResultEvents = append(toolResultEvents, ev.data)
			case "done":
				done = true
			}
		case <-deadline:
			t.Fatal("timeout waiting for SSE events")
		}
	}

	// Verify we got at least one tool_use and one tool_result.
	if len(toolUseEvents) == 0 {
		t.Fatal("expected at least one tool_use SSE event")
	}
	if len(toolResultEvents) == 0 {
		t.Fatal("expected at least one tool_result SSE event")
	}

	// Verify tool_use data has "tool" field.
	var tu map[string]string
	if err := json.Unmarshal([]byte(toolUseEvents[0]), &tu); err != nil {
		t.Fatalf("parsing tool_use data: %v", err)
	}
	if tu["tool"] == "" {
		t.Fatal("tool_use event missing 'tool' field")
	}
	if tu["id"] == "" {
		t.Fatal("tool_use event missing 'id' field")
	}

	// Verify tool_result data has "output" field.
	var tr map[string]string
	if err := json.Unmarshal([]byte(toolResultEvents[0]), &tr); err != nil {
		t.Fatalf("parsing tool_result data: %v", err)
	}
	if tr["output"] == "" {
		t.Fatal("tool_result event missing 'output' field")
	}
}

// ---------------------------------------------------------------------------
// Test 33: Chat page serves full HTML with template data
// ---------------------------------------------------------------------------
//
// GET /chat through the proxy should return a full HTML page with viewport
// meta, CSS custom properties, and injected auth config.

func TestChatPageServesFullHTML(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	ports, release := reservePorts(t, 3)
	apiPort, appPort, intPort := ports[0], ports[1], ports[2]

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort, release)
	_ = orch

	mustDeploy(t, apiPort, repo.CommitA)

	code, body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/chat", appPort))
	if code != 200 {
		t.Fatalf("expected 200 for /chat, got %d", code)
	}

	// Must be a full HTML document.
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Fatal("/chat response missing <!DOCTYPE html>")
	}

	// Must have viewport meta for mobile.
	if !strings.Contains(body, "viewport") {
		t.Fatal("/chat response missing viewport meta tag")
	}

	// Must have CSS custom properties.
	if !strings.Contains(body, "--sm-bg") {
		t.Fatal("/chat response missing CSS custom properties")
	}

	// Must reference the config endpoint.
	if !strings.Contains(body, "/chat/config") {
		t.Fatal("/chat response missing /chat/config fetch")
	}

	// Must NOT contain Go template syntax.
	if strings.Contains(body, "{{") {
		t.Fatal("/chat response contains Go template syntax")
	}

	// /chat/config must return valid JSON with auth config.
	configCode, configBody := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/chat/config", appPort))
	if configCode != 200 {
		t.Fatalf("expected 200 for /chat/config, got %d", configCode)
	}
	if !strings.Contains(configBody, `"authMode"`) {
		t.Fatalf("/chat/config missing authMode: %s", configBody)
	}
}
