// Fake Claude CLI for slot-machine spec tests.
//
// Outputs stream-json events at 1-second intervals, simulating an agent
// that's working on a task. Accepts the same flags as the real Claude CLI
// so the orchestrator can spawn it identically.
//
// Build:
//
//	go build -o spec/testagent/testagent ./spec/testagent/
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	_ = flag.String("output-format", "", "output format (ignored, always stream-json)")
	prompt := flag.String("p", "", "prompt")
	resume := flag.String("resume", "", "session ID to resume")
	_ = flag.String("cwd", "", "working directory")
	_ = flag.String("system-prompt", "", "system prompt")
	_ = flag.String("allowedTools", "", "allowed tools")
	interval := flag.Int("interval", 200, "milliseconds between events")
	duration := flag.Int("duration", 10, "number of events to emit")
	flag.Parse()

	sessionID := fmt.Sprintf("test-session-%d", time.Now().UnixNano())
	if *resume != "" {
		sessionID = *resume
	}

	// Init event.
	fmt.Fprintf(os.Stdout, "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"%s\"}\n", sessionID)

	// Assistant events at interval, with tool events interleaved.
	for i := 0; i < *duration; i++ {
		time.Sleep(time.Duration(*interval) * time.Millisecond)
		text := fmt.Sprintf("working on: %s (%d/%d)", *prompt, i+1, *duration)
		if i == 0 {
			text = fmt.Sprintf("[[TITLE: %s]]\n%s", *prompt, text)
		}
		fmt.Fprintf(os.Stdout, "{\"type\":\"assistant\",\"text\":%q}\n", text)

		// Emit tool events after the first two assistant messages.
		if i == 0 {
			time.Sleep(time.Duration(*interval) * time.Millisecond)
			fmt.Fprintln(os.Stdout, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool_001","name":"Edit","input":{}}}`)
			time.Sleep(time.Duration(*interval) * time.Millisecond)
			fmt.Fprintln(os.Stdout, `{"type":"tool_result","tool_use_id":"tool_001","content":"File edited successfully"}`)
		}
		if i == 1 {
			time.Sleep(time.Duration(*interval) * time.Millisecond)
			fmt.Fprintln(os.Stdout, `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool_002","name":"Bash","input":{}}}`)
			time.Sleep(time.Duration(*interval) * time.Millisecond)
			fmt.Fprintln(os.Stdout, `{"type":"tool_result","tool_use_id":"tool_002","content":"$ git status\nnothing to commit"}`)
		}
	}

	// Result event.
	fmt.Fprintln(os.Stdout, "{\"type\":\"result\",\"subtype\":\"success\"}")
}
