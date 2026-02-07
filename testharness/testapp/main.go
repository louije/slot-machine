// Dummy test app for the orchestrator test harness.
//
// A tiny HTTP server the orchestrator can deploy. It serves a public endpoint
// and internal control/health endpoints on separate ports. Various flags let
// tests simulate unhealthy starts, slow boots, and processes that refuse to die.
//
// Port configuration follows the same pattern as real apps: the orchestrator
// sets PORT and INTERNAL_PORT env vars (like systemd's EnvironmentFile would).
// Flags override env vars for manual testing.
//
// Build:
//   go build -o testharness/testapp/testapp ./testharness/testapp/
//
// Usage:
//   PORT=3001 INTERNAL_PORT=3901 ./testapp [--start-unhealthy] [--boot-delay 3]
//   ./testapp --port 3001 [--internal-port 3901]   # flags override env
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// envInt reads an integer from an environment variable, returning 0 if unset.
func envInt(key string) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func main() {
	port := flag.Int("port", envInt("PORT"), "Public port (or set PORT env var)")
	internalPort := flag.Int("internal-port", envInt("INTERNAL_PORT"), "Internal port (or set INTERNAL_PORT env var)")
	startUnhealthy := flag.Bool("start-unhealthy", false, "Start with health check returning 503")
	bootDelay := flag.Int("boot-delay", 0, "Seconds to wait before starting HTTP servers")
	flag.Parse()

	if *port == 0 {
		fmt.Fprintln(os.Stderr, "error: port required (set PORT env var or use --port)")
		os.Exit(1)
	}

	if *internalPort == 0 {
		*internalPort = *port + 900
	}

	if *bootDelay > 0 {
		time.Sleep(time.Duration(*bootDelay) * time.Second)
	}

	// Shared state guarded by a mutex.
	var mu sync.Mutex
	healthy := !*startUnhealthy

	// --- Public server ---

	pubMux := http.NewServeMux()

	// GET / — simple response proving the app is running and on which port.
	pubMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"port":   *port,
		})
	})

	// --- Internal server ---

	intMux := http.NewServeMux()

	// GET /healthz — returns 200 when healthy, 503 when not.
	intMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := healthy
		mu.Unlock()
		if h {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "ok")
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "unhealthy")
		}
	})

	// POST /control/unhealthy — makes /healthz return 503.
	intMux.HandleFunc("/control/unhealthy", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		healthy = false
		mu.Unlock()
		fmt.Fprint(w, "now unhealthy")
	})

	// POST /control/healthy — restores /healthz to 200.
	intMux.HandleFunc("/control/healthy", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		healthy = true
		mu.Unlock()
		fmt.Fprint(w, "now healthy")
	})

	// POST /control/hang — makes the process ignore SIGTERM (for drain timeout test).
	intMux.HandleFunc("/control/hang", func(w http.ResponseWriter, r *http.Request) {
		// Catch SIGTERM and do nothing — the process will only die to SIGKILL.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		go func() {
			for range sig {
				// swallow
			}
		}()
		fmt.Fprint(w, "now ignoring SIGTERM")
	})

	// POST /control/crash — exits the process immediately.
	intMux.HandleFunc("/control/crash", func(w http.ResponseWriter, r *http.Request) {
		// Flush the response then die.
		fmt.Fprint(w, "crashing")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			time.Sleep(50 * time.Millisecond)
			os.Exit(1)
		}()
	})

	// Start both servers.
	go func() {
		if err := http.ListenAndServe(fmt.Sprintf(":%d", *internalPort), intMux); err != nil {
			fmt.Fprintf(os.Stderr, "internal server error: %v\n", err)
			os.Exit(1)
		}
	}()

	fmt.Printf("testapp listening: public=:%d internal=:%d\n", *port, *internalPort)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), pubMux); err != nil {
		fmt.Fprintf(os.Stderr, "public server error: %v\n", err)
		os.Exit(1)
	}
}
