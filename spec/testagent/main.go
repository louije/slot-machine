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

	// Assistant events at interval.
	for i := 0; i < *duration; i++ {
		time.Sleep(time.Duration(*interval) * time.Millisecond)
		fmt.Fprintf(os.Stdout, "{\"type\":\"assistant\",\"text\":\"working on: %s (%d/%d)\"}\n", *prompt, i+1, *duration)
	}

	// Result event.
	fmt.Fprintln(os.Stdout, "{\"type\":\"result\",\"subtype\":\"success\"}")
}
