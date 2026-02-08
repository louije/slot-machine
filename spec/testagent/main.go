// Fake Claude CLI for slot-machine spec tests.
//
// Outputs stream-json events matching the real Claude CLI format.
// Accepts the same flags as the real Claude CLI so the orchestrator
// can spawn it identically.
//
// Build:
//
//	go build -o spec/testagent/testagent ./spec/testagent/
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func emit(v any) {
	data, _ := json.Marshal(v)
	fmt.Fprintln(os.Stdout, string(data))
}

func main() {
	_ = flag.String("output-format", "", "output format (ignored, always stream-json)")
	prompt := flag.String("p", "", "prompt")
	resume := flag.String("resume", "", "session ID to resume")
	_ = flag.String("cwd", "", "working directory")
	_ = flag.String("system-prompt", "", "system prompt")
	_ = flag.String("allowedTools", "", "allowed tools")
	_ = flag.String("allowed-tools", "", "allowed tools (alt form)")
	_ = flag.Bool("dangerously-skip-permissions", false, "bypass permissions")
	interval := flag.Int("interval", 200, "milliseconds between events")
	duration := flag.Int("duration", 10, "number of events to emit")
	flag.Parse()

	sessionID := fmt.Sprintf("test-session-%d", time.Now().UnixNano())
	if *resume != "" {
		sessionID = *resume
	}

	delay := func() { time.Sleep(time.Duration(*interval) * time.Millisecond) }

	// Init event.
	emit(map[string]any{
		"type": "system", "subtype": "init", "session_id": sessionID,
	})

	for i := 0; i < *duration; i++ {
		delay()
		text := fmt.Sprintf("working on: %s (%d/%d)", *prompt, i+1, *duration)
		if i == 0 {
			text = fmt.Sprintf("[[TITLE: %s]]\n%s", *prompt, text)
		}

		// After first text, include a tool_use in the same assistant message.
		if i == 0 {
			emit(map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": text},
						map[string]any{"type": "tool_use", "id": "tool_001", "name": "Edit", "input": map[string]any{}},
					},
				},
				"session_id": sessionID,
			})
			delay()
			// Tool result as user event.
			emit(map[string]any{
				"type": "user",
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "tool_result", "tool_use_id": "tool_001", "content": "File edited successfully"},
					},
				},
			})
			continue
		}

		if i == 1 {
			emit(map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": text},
						map[string]any{"type": "tool_use", "id": "tool_002", "name": "Bash", "input": map[string]any{}},
					},
				},
				"session_id": sessionID,
			})
			delay()
			emit(map[string]any{
				"type": "user",
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "tool_result", "tool_use_id": "tool_002", "content": "$ git status\nnothing to commit"},
					},
				},
			})
			continue
		}

		// Regular text-only assistant message.
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": text},
				},
			},
			"session_id": sessionID,
		})
	}

	// Result event.
	emit(map[string]any{
		"type":    "result",
		"subtype": "success",
		"result":  fmt.Sprintf("Done working on: %s", *prompt),
		"usage": map[string]any{
			"input_tokens":                100,
			"output_tokens":               50,
			"cache_read_input_tokens":     80,
			"cache_creation_input_tokens": 20,
		},
	})
}
