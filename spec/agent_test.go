// Agent service and proxy intercept spec tests.
//
// These validate two architectural claims from agent.md:
//
// 1. Proxy intercept: the reverse proxy handles /agent/* and /chat paths
//    internally (slot-machine serves them), forwarding everything else to
//    the app. Same origin, same port, no CORS.
//
// 2. Deploy-through: the agent process is a child of slot-machine, not the
//    app. When the app is swapped during a deploy, the agent keeps running
//    and the SSE connection through the proxy stays connected.
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

	apiPort := freePort(t)
	appPort := freePort(t) // proxy listens here
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Deploy an app.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

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

	apiPort := freePort(t)
	appPort := freePort(t) // proxy listens here
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Deploy so the proxy is active.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

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

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Deploy so the proxy is active.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

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

	apiPort := freePort(t)
	appPort := freePort(t) // proxy listens here
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestratorWithAgent(t, bin, contract, repo.Dir, apiPort, agentBin)
	_ = orch

	// 1. Deploy app A.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy A failed")
	}

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

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestratorWithAgent(t, bin, contract, repo.Dir, apiPort, agentBin)
	_ = orch

	// Deploy so the agent service is active.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

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
// Test 31: HMAC auth rejects unauthenticated requests
// ---------------------------------------------------------------------------
//
// When agent_auth is "hmac" (default), requests to /agent/* without a valid
// X-SlotMachine-User header should get 401. The /chat path should still be
// accessible (it's a static HTML page, not an API).

func TestHMACAuthRejectsUnauthenticated(t *testing.T) {
	t.Parallel()
	bin := orchestratorBinary(t)
	appBin := testappBinary(t)

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	// Write a contract with hmac auth (the default — just omit agent_auth).
	contract := writeTestContractWithAuth(t, t.TempDir(), appPort, intPort, 0, "hmac")

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	// Deploy so the proxy is active.
	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", appPort)

	// GET /agent/conversations without auth header — should be 401.
	code, _ := httpGet(t, proxyURL+"/agent/conversations")
	if code != 401 {
		t.Fatalf("expected 401 for unauthenticated GET /agent/conversations, got %d", code)
	}

	// POST /agent/conversations without auth header — should be 401.
	postCode := httpPost(t, proxyURL+"/agent/conversations")
	if postCode != 401 {
		t.Fatalf("expected 401 for unauthenticated POST /agent/conversations, got %d", postCode)
	}

	// GET /chat should still return 200 HTML (not auth-protected).
	code, body := httpGet(t, proxyURL+"/chat")
	if code != 200 {
		t.Fatalf("expected 200 for /chat, got %d", code)
	}
	if !strings.Contains(body, "<html") {
		t.Fatalf("expected HTML for /chat, got: %s", body)
	}
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

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestratorWithAgent(t, bin, contract, repo.Dir, apiPort, agentBin)
	_ = orch

	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", appPort)

	// Create conversation and send message.
	resp, err := http.Post(proxyURL+"/agent/conversations", "application/json", nil)
	if err != nil {
		t.Fatalf("creating conversation: %v", err)
	}
	var conv struct{ ID string `json:"id"` }
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

	apiPort := freePort(t)
	appPort := freePort(t)
	intPort := freePort(t)

	repo := setupTestRepo(t, appBin, appPort, intPort)
	contract := writeTestContract(t, t.TempDir(), appPort, intPort, 0)

	orch := startOrchestrator(t, bin, contract, repo.Dir, apiPort)
	_ = orch

	dr, _ := deploy(t, apiPort, repo.CommitA)
	if !dr.Success {
		t.Fatal("deploy failed")
	}

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
