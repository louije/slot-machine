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
