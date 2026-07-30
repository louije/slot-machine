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
	"strconv"
	"time"
)

func emit(v any) {
	data, _ := json.Marshal(v)
	fmt.Fprintln(os.Stdout, string(data))
}

// Every flag slot-machine passes must be declared here, even the ones this
// double ignores. Go's flag package exits 2 on an unknown flag, so a flag added
// to the real invocation and not mirrored here turns the double into a process
// that prints usage and dies — which is exactly what happened when --verbose was
// added: three agent spec tests failed for months with no output to explain it.
//
// If you add a flag in buildAgentArgs, add it here in the same commit.
func main() {
	_ = flag.String("output-format", "", "output format (ignored, always stream-json)")
	_ = flag.Bool("verbose", false, "verbose output")
	prompt := flag.String("p", "", "prompt")
	resume := flag.String("resume", "", "session ID to resume")
	_ = flag.String("session-id", "", "session ID for a fresh session")
	_ = flag.String("cwd", "", "working directory")
	_ = flag.String("model", "", "model")
	_ = flag.String("system-prompt", "", "system prompt (replaces the built-in)")
	_ = flag.String("append-system-prompt", "", "system prompt (appended)")
	_ = flag.String("allowedTools", "", "allowed tools")
	_ = flag.String("allowed-tools", "", "allowed tools (alt form)")
	_ = flag.String("disallowedTools", "", "disallowed tools")
	_ = flag.String("permission-mode", "", "permission mode")
	_ = flag.String("settings", "", "settings file")
	_ = flag.Bool("dangerously-skip-permissions", false, "bypass permissions")
	interval := flag.Int("interval", 200, "milliseconds between events")
	duration := flag.Int("duration", 10, "number of events to emit")

	// slot-machine passes the prompt as `-p -- <text>` so a message beginning
	// with "-" is not parsed as a flag. The real CLI treats the bare "--" as
	// end-of-flags and takes the rest verbatim; Go's flag package does not — it
	// consumes "--" as -p's value and then chokes on a prompt starting with "-".
	// Pull the prompt out by hand so the double behaves like the real thing,
	// which is the only way a test can cover that case at all.
	args := os.Args[1:]
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "-p" && args[i+1] == "--" {
			*prompt = args[i+2]
			args = append(args[:i:i], args[i+3:]...)
			break
		}
	}
	flag.CommandLine.Parse(args)
	if *prompt == "" && flag.NArg() > 0 {
		*prompt = flag.Arg(0)
	}

	// Behaviour switches, read from the environment because the real CLI has no
	// equivalent flags and slot-machine builds argv itself. The daemon passes
	// its own environment through to the agent, so a test sets these on the
	// daemon process.
	//
	// TESTAGENT_DURATION is what lets a test pin down how long the agent lives
	// instead of racing it. A test that needs the agent to still be running
	// after some other operation completes must not depend on that operation
	// being faster than a fixed event count.
	if d := os.Getenv("TESTAGENT_DURATION"); d != "" {
		if n, err := strconv.Atoi(d); err == nil {
			*duration = n
		}
	}
	if iv := os.Getenv("TESTAGENT_INTERVAL"); iv != "" {
		if n, err := strconv.Atoi(iv); err == nil {
			*interval = n
		}
	}
	if code := os.Getenv("TESTAGENT_EXIT_CODE"); code != "" {
		if msg := os.Getenv("TESTAGENT_STDERR"); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		n, _ := strconv.Atoi(code)
		os.Exit(n)
	}
	if d := os.Getenv("TESTAGENT_HANG_MS"); d != "" {
		ms, _ := strconv.Atoi(d)
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}

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
