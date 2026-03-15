package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type agentWork struct {
	convID    string
	message   string
	sessionID string
	bin       string
	args      []string
	dir       string
	env       []string
	resultCh  chan error
}

type runningAgent struct {
	cmd      *exec.Cmd
	convID   string
	eventSeq uint64
	mu       sync.Mutex
	cond     *sync.Cond
	done     chan struct{}
}

type agentManager struct {
	store   *agentStore
	workCh  chan agentWork
	running map[string]*runningAgent
	mu      sync.Mutex
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

func newAgentManager(store *agentStore) *agentManager {
	m := &agentManager{
		store:   store,
		workCh:  make(chan agentWork, 16),
		running: make(map[string]*runningAgent),
		stopCh:  make(chan struct{}),
	}
	m.wg.Add(1)
	go m.loop()
	return m
}

func (m *agentManager) loop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stopCh:
			return
		case work := <-m.workCh:
			m.mu.Lock()
			if _, running := m.running[work.convID]; running {
				m.mu.Unlock()
				work.resultCh <- fmt.Errorf("agent already running")
				continue
			}
			m.mu.Unlock()
			work.resultCh <- nil
			m.wg.Add(1)
			go m.runAgent(work)
		}
	}
}

func (m *agentManager) stop() {
	close(m.stopCh)
	m.mu.Lock()
	for _, ra := range m.running {
		if ra.cmd != nil && ra.cmd.Process != nil {
			ra.cmd.Process.Kill()
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *agentManager) enqueue(work agentWork) error {
	work.resultCh = make(chan error, 1)
	select {
	case m.workCh <- work:
		return <-work.resultCh
	case <-m.stopCh:
		return fmt.Errorf("manager stopped")
	}
}

func (m *agentManager) runAgent(work agentWork) {
	defer m.wg.Done()

	ra := &runningAgent{
		convID: work.convID,
		done:   make(chan struct{}),
	}
	ra.cond = sync.NewCond(&ra.mu)

	m.mu.Lock()
	m.running[work.convID] = ra
	m.mu.Unlock()

	m.store.setConversationStatus(work.convID, "running")

	cmd := exec.Command(work.bin, work.args...)
	cmd.Dir = work.dir
	if work.env != nil {
		cmd.Env = work.env
	}
	ra.cmd = cmd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		errContent, _ := json.Marshal(map[string]string{
			"content": fmt.Sprintf("Failed to start agent: %v", err),
		})
		m.store.setConversationStatus(work.convID, "error")
		m.storeAndBroadcast(work.convID, ra, "system", string(errContent))
		m.cleanup(ra)
		return
	}

	if err := cmd.Start(); err != nil {
		errContent, _ := json.Marshal(map[string]string{
			"content": fmt.Sprintf("Failed to start agent: %v", err),
		})
		m.store.setConversationStatus(work.convID, "error")
		m.storeAndBroadcast(work.convID, ra, "system", string(errContent))
		m.cleanup(ra)
		return
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	var hitCorruption bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "tool_use ids must be unique") {
			hitCorruption = true
		}
		m.processLine(work.convID, ra, line)
	}

	exitErr := cmd.Wait()

	// Session corruption recovery: retry once without --resume.
	if hitCorruption && work.sessionID != "" {
		errContent, _ := json.Marshal(map[string]string{
			"content": "Session corrupted, restarting without history.",
		})
		m.storeAndBroadcast(work.convID, ra, "system", string(errContent))

		var retryArgs []string
		for i := 0; i < len(work.args); i++ {
			if work.args[i] == "--resume" {
				i++ // skip session ID argument
				continue
			}
			retryArgs = append(retryArgs, work.args[i])
		}
		retryCmd := exec.Command(work.bin, retryArgs...)
		retryCmd.Dir = work.dir
		retryCmd.Env = work.env
		ra.cmd = retryCmd

		if retryOut, err := retryCmd.StdoutPipe(); err == nil {
			if err := retryCmd.Start(); err == nil {
				retryScanner := bufio.NewScanner(retryOut)
				retryScanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
				for retryScanner.Scan() {
					m.processLine(work.convID, ra, retryScanner.Text())
				}
				exitErr = retryCmd.Wait()
			}
		}
	}

	if exitErr != nil {
		errContent, _ := json.Marshal(map[string]string{
			"content": fmt.Sprintf("Agent exited with error: %v", exitErr),
		})
		m.store.setConversationStatus(work.convID, "error")
		m.storeAndBroadcast(work.convID, ra, "system", string(errContent))
	} else {
		m.store.setConversationStatus(work.convID, "idle")
	}

	// Broadcast final event so SSE clients know to close.
	ra.mu.Lock()
	ra.eventSeq++
	ra.mu.Unlock()
	ra.cond.Broadcast()

	m.cleanup(ra)
}

func (m *agentManager) cleanup(ra *runningAgent) {
	m.mu.Lock()
	delete(m.running, ra.convID)
	m.mu.Unlock()
	close(ra.done)
	ra.cond.Broadcast()
}

func (m *agentManager) getRunning(convID string) *runningAgent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[convID]
}

func (m *agentManager) cancel(convID string) error {
	m.mu.Lock()
	ra, ok := m.running[convID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("no running agent for %s", convID)
	}
	if ra.cmd != nil && ra.cmd.Process != nil {
		ra.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-ra.done:
			return nil
		case <-time.After(5 * time.Second):
			ra.cmd.Process.Kill()
		}
	}
	<-ra.done
	return nil
}

func (m *agentManager) processLine(convID string, ra *runningAgent, line string) {
	var raw map[string]any
	if json.Unmarshal([]byte(line), &raw) != nil {
		return
	}

	evtType, _ := raw["type"].(string)

	switch evtType {
	case "system":
		if sub, _ := raw["subtype"].(string); sub == "init" {
			if sid, ok := raw["session_id"].(string); ok {
				m.store.updateSessionID(convID, sid)
			}
		}
		m.storeAndBroadcast(convID, ra, "system", line)

	case "assistant":
		var blocks []any
		if msg, ok := raw["message"].(map[string]any); ok {
			blocks, _ = msg["content"].([]any)
		}

		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if bt, _ := block["type"].(string); bt == "tool_use" {
				toolName, _ := block["name"].(string)
				toolID, _ := block["id"].(string)
				data, _ := json.Marshal(map[string]string{"tool": toolName, "id": toolID})
				m.storeAndBroadcast(convID, ra, "tool_use", string(data))
			}
		}

		var text string
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if bt, _ := block["type"].(string); bt == "text" {
				if t, _ := block["text"].(string); t != "" {
					text += t
				}
			}
		}

		// IMPORTANT: Use "match" not "m" to avoid shadowing the receiver.
		if match := titlePattern.FindStringSubmatch(text); match != nil {
			m.store.updateTitle(convID, strings.TrimSpace(match[1]))
			text = strings.TrimSpace(titlePattern.ReplaceAllString(text, ""))
		}

		if text != "" {
			data, _ := json.Marshal(map[string]string{"content": text})
			m.storeAndBroadcast(convID, ra, "assistant", string(data))
		}

	case "user":
		var blocks []any
		if msg, ok := raw["message"].(map[string]any); ok {
			blocks, _ = msg["content"].([]any)
		}
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if bt, _ := block["type"].(string); bt == "tool_result" {
				toolID, _ := block["tool_use_id"].(string)
				content, _ := block["content"].(string)
				data, _ := json.Marshal(map[string]string{"id": toolID, "output": content})
				m.storeAndBroadcast(convID, ra, "tool_result", string(data))
			}
		}

	case "result":
		var inputTok, outputTok, cacheRead, cacheWrite float64
		if usage, ok := raw["usage"].(map[string]any); ok {
			inputTok, _ = usage["input_tokens"].(float64)
			outputTok, _ = usage["output_tokens"].(float64)
			cacheRead, _ = usage["cache_read_input_tokens"].(float64)
			cacheWrite, _ = usage["cache_creation_input_tokens"].(float64)
		}
		m.store.addUsage(convID, int(inputTok), int(outputTok), int(cacheRead), int(cacheWrite))

		if resultText, _ := raw["result"].(string); resultText != "" {
			if match := titlePattern.FindStringSubmatch(resultText); match != nil {
				m.store.updateTitle(convID, strings.TrimSpace(match[1]))
			}
		}

		m.storeAndBroadcast(convID, ra, "done", line)
	}
}

func (m *agentManager) storeAndBroadcast(convID string, ra *runningAgent, msgType, content string) {
	m.store.addMessage(convID, msgType, content)
	ra.mu.Lock()
	ra.eventSeq++
	ra.mu.Unlock()
	ra.cond.Broadcast()
}
