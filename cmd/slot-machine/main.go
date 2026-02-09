// slot-machine — blue-green deploys on a single machine.
//
// Usage:
//
//	slot-machine init                  # scaffold slot-machine.json + update .gitignore
//	slot-machine start [flags]         # start daemon, auto-deploy HEAD
//	slot-machine deploy [commit]       # tell running daemon to deploy (defaults to HEAD)
//	slot-machine rollback              # tell running daemon to rollback
//	slot-machine status                # get status from running daemon
//	slot-machine install               # copy binary to ~/.local/bin
//	slot-machine update                # update to latest GitHub release
//
// Build:
//
//	go build -o slot-machine ./cmd/slot-machine/
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// Version is injected at build time via -ldflags="-X main.Version=v1.0.0".
var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: slot-machine <command> [args]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "commands:")
		fmt.Fprintln(os.Stderr, "  init       scaffold slot-machine.json")
		fmt.Fprintln(os.Stderr, "  start      start the daemon")
		fmt.Fprintln(os.Stderr, "  deploy     deploy a commit")
		fmt.Fprintln(os.Stderr, "  rollback   rollback to previous")
		fmt.Fprintln(os.Stderr, "  status     show current status")
		fmt.Fprintln(os.Stderr, "  install    copy binary to ~/.local/bin")
		fmt.Fprintln(os.Stderr, "  update     update to latest GitHub release")
		fmt.Fprintln(os.Stderr, "  version    print version info")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "init":
		cmdInit()
	case "start":
		cmdStart(os.Args[2:])
	case "deploy":
		cmdDeploy(os.Args[2:])
	case "rollback":
		cmdRollback()
	case "status":
		cmdStatus()
	case "install":
		cmdInstall()
	case "update":
		cmdUpdate()
	case "version":
		fmt.Println(Version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Subcommand: start
// ---------------------------------------------------------------------------

func cmdStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	configPath := fs.String("config", "", "path to slot-machine.json (default: ./slot-machine.json)")
	repoDir := fs.String("repo", "", "path to git repo (default: .)")
	dataDir := fs.String("data", "", "path to data directory (default: <repo>/.slot-machine)")
	port := fs.Int("port", 0, "API listen port (default: config api_port or 9100)")
	_ = fs.Bool("no-proxy", false, "ignored (kept for backward compatibility)")
	fs.Parse(args)

	cwd, _ := os.Getwd()

	if *configPath == "" {
		*configPath = filepath.Join(cwd, "slot-machine.json")
	}
	if *repoDir == "" {
		*repoDir = cwd
	}
	if *dataDir == "" {
		*dataDir = filepath.Join(*repoDir, ".slot-machine")
	}

	cfgData, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot read %s\n", *configPath)
		fmt.Fprintln(os.Stderr, "run 'slot-machine init' to create it")
		os.Exit(1)
	}
	var cfg config
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing config: %v\n", err)
		os.Exit(1)
	}

	apiPort := 9100
	if cfg.APIPort != 0 {
		apiPort = cfg.APIPort
	}
	if *port != 0 {
		apiPort = *port
	}

	absRepo, err := filepath.Abs(*repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error resolving repo path: %v\n", err)
		os.Exit(1)
	}

	os.MkdirAll(*dataDir, 0755)

	appProxyAddr := ""
	if cfg.Port != 0 {
		appProxyAddr = fmt.Sprintf(":%d", cfg.Port)
	}
	intProxyAddr := ""
	if cfg.InternalPort != 0 && cfg.InternalPort != cfg.Port {
		intProxyAddr = fmt.Sprintf(":%d", cfg.InternalPort)
	}

	// Auth setup.
	authMode := cfg.AgentAuth
	if authMode == "" {
		authMode = "hmac"
	}
	var authSecret string
	if authMode == "hmac" {
		secretBytes := make([]byte, 32)
		if _, err := rand.Read(secretBytes); err != nil {
			fmt.Fprintf(os.Stderr, "error generating auth secret: %v\n", err)
			os.Exit(1)
		}
		authSecret = hex.EncodeToString(secretBytes)
	}
	fmt.Printf("agent auth: %s\n", authMode)

	store, err := openAgentStore(filepath.Join(*dataDir, "agent.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening agent store: %v\n", err)
		os.Exit(1)
	}

	agent := &agentService{
		store:      store,
		sessions:   make(map[string]*agentSession),
		agentBin:   os.Getenv("SLOT_MACHINE_AGENT_BIN"),
		stagingDir: filepath.Join(*dataDir, "slot-staging"),
		authMode:     authMode,
		authSecret:   authSecret,
		allowedTools: cfg.AgentAllowedTools,
		chatTitle:      cfg.ChatTitle,
		chatAccent:     cfg.ChatAccent,
		envFunc: func() []string {
			env := os.Environ()
			if cfg.EnvFile != "" {
				envPath := cfg.EnvFile
				if !filepath.IsAbs(envPath) {
					envPath = filepath.Join(absRepo, envPath)
				}
				if extra, err := loadEnvFile(envPath); err == nil {
					env = append(env, extra...)
				}
			}
			return env
		},
	}

	o := &orchestrator{
		cfg:        cfg,
		repoDir:    absRepo,
		dataDir:    *dataDir,
		authSecret: authSecret,
		appProxy:   newDynamicProxy(appProxyAddr, agent),
		intProxy:   newDynamicProxy(intProxyAddr, nil),
	}

	// Recover state from symlinks, or auto-deploy HEAD.
	o.recoverState()
	if o.liveSlot == nil {
		commit, err := gitHeadCommit(absRepo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot determine HEAD: %v\n", err)
		} else {
			fmt.Printf("auto-deploying HEAD (%s)...\n", shortHash(commit))
			resp, _ := o.doDeploy(commit)
			if resp.Success {
				fmt.Printf("deployed %s to %s\n", shortHash(resp.Commit), resp.Slot)
			} else {
				fmt.Fprintf(os.Stderr, "auto-deploy failed: %s\n", resp.Error)
			}
		}
	}

	// API server.
	apiAddr := fmt.Sprintf(":%d", apiPort)
	apiSrv := &http.Server{Addr: apiAddr, Handler: o}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		fmt.Println("\nshutting down...")
		o.drainAll()
		o.appProxy.shutdown()
		o.intProxy.shutdown()
		store.close()
		apiSrv.Shutdown(context.Background())
	}()

	fmt.Printf("slot-machine listening on %s\n", apiAddr)
	if err := apiSrv.ListenAndServe(); err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Subcommand: deploy
// ---------------------------------------------------------------------------

func cmdDeploy(args []string) {
	commit := ""
	if len(args) > 0 {
		commit = args[0]
	}

	if commit == "" {
		cwd, _ := os.Getwd()
		c, err := gitHeadCommit(cwd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot determine HEAD commit: %v\n", err)
			os.Exit(1)
		}
		commit = c
	}

	port := readAPIPort()
	body, _ := json.Marshal(map[string]string{"commit": commit})
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/deploy", port),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot reach slot-machine daemon: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var dr deployResponse
	json.NewDecoder(resp.Body).Decode(&dr)

	if dr.Success {
		fmt.Printf("deployed %s to %s\n", shortHash(dr.Commit), dr.Slot)
	} else {
		fmt.Fprintf(os.Stderr, "deploy failed: %s\n", dr.Error)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Subcommand: rollback
// ---------------------------------------------------------------------------

func cmdRollback() {
	port := readAPIPort()
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/rollback", port),
		"application/json",
		nil,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot reach slot-machine daemon: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var rr rollbackResponse
	json.NewDecoder(resp.Body).Decode(&rr)

	if rr.Success {
		fmt.Printf("rolled back to %s (%s)\n", shortHash(rr.Commit), rr.Slot)
	} else {
		fmt.Fprintf(os.Stderr, "rollback failed: %s\n", rr.Error)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Subcommand: status
// ---------------------------------------------------------------------------

func cmdStatus() {
	port := readAPIPort()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/status", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot reach slot-machine daemon: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var sr statusResponse
	json.NewDecoder(resp.Body).Decode(&sr)

	healthy := "no"
	if sr.Healthy {
		healthy = "yes"
	}

	fmt.Printf("live:     %s  %s  healthy=%s\n", sr.LiveSlot, sr.LiveCommit, healthy)
	if sr.PreviousSlot != "" {
		fmt.Printf("previous: %s  %s\n", sr.PreviousSlot, sr.PreviousCommit)
	}
	if sr.StagingDir != "" {
		fmt.Printf("staging:  %s\n", sr.StagingDir)
	}
	if sr.LastDeployTime != "" {
		fmt.Printf("last deploy: %s\n", sr.LastDeployTime)
	}
}

// ---------------------------------------------------------------------------
// Subcommand: install
// ---------------------------------------------------------------------------

func cmdInstall() {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine own path: %v\n", err)
		os.Exit(1)
	}
	// Resolve symlinks so we copy the real binary.
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine home directory: %v\n", err)
		os.Exit(1)
	}

	destDir := filepath.Join(home, ".local", "bin")
	os.MkdirAll(destDir, 0755)
	dest := filepath.Join(destDir, "slot-machine")

	// Read source binary.
	data, err := os.ReadFile(self)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading binary: %v\n", err)
		os.Exit(1)
	}

	// Write to temp file in same dir, then rename (atomic).
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", tmp, err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "error installing: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("installed %s\n", dest)

	// Check if destDir is in PATH.
	pathEnv := os.Getenv("PATH")
	inPath := false
	for _, p := range filepath.SplitList(pathEnv) {
		if p == destDir {
			inPath = true
			break
		}
	}
	if !inPath {
		fmt.Printf("\nnote: %s is not in your PATH\n", destDir)
		fmt.Printf("add this to your shell profile:\n")
		fmt.Printf("  export PATH=\"%s:$PATH\"\n", destDir)
	}
}

func shortHash(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func readAPIPort() int {
	cwd, _ := os.Getwd()
	dir := cwd
	for {
		data, err := os.ReadFile(filepath.Join(dir, "slot-machine.json"))
		if err == nil {
			var cfg config
			json.Unmarshal(data, &cfg)
			if cfg.APIPort != 0 {
				return cfg.APIPort
			}
			return 9100
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	fmt.Fprintln(os.Stderr, "error: cannot find slot-machine.json in current or parent directories")
	os.Exit(1)
	return 0
}
