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
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"slot-machine/internal/agent"
	"slot-machine/internal/agent/store"
	"slot-machine/internal/config"
	"slot-machine/internal/orchestrator"
)

// Version is injected at build time via -ldflags="-X main.Version=v1.0.0".
var Version = "dev"

func main() {
	// One prefix, no timestamps: this runs under systemd or launchd, both of
	// which already stamp every line.
	log.SetFlags(0)
	log.SetPrefix("slot-machine: ")

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

	cfg, err := config.Load(*configPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "error: cannot read %s\n", *configPath)
			fmt.Fprintln(os.Stderr, "run 'slot-machine init' to create it")
		} else {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(1)
	}

	apiPort := cfg.APIPort
	if *port != 0 {
		apiPort = *port
	}

	absRepo, err := filepath.Abs(*repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error resolving repo path: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating data directory: %v\n", err)
		os.Exit(1)
	}

	// Bind the API port before anything else, and serve on it last.
	//
	// Binding early is not cosmetic. Deploys allocate their slot ports from the
	// ephemeral range, and until this listener exists the API port is simply
	// unclaimed — so a slot's app process could be handed it, and then the daemon
	// could not bind its own API at all. Reserving it up front closes that window
	// while still letting startup finish before any request is served, so a
	// reachable API continues to mean "startup is done".
	//
	// Loopback, always, with no config field to widen it. POST /deploy and POST
	// /rollback are unauthenticated by design — the CLI is a thin client over
	// them — so the bind address is the only thing standing between a promotion
	// and everyone else on the network. This used to be ":%d", which meant every
	// interface, while both docs/agent.md and the spec claimed localhost.
	//
	// It is also the recovery path when the agent surface is unreachable: with
	// an authenticating proxy down, `slot-machine status` and `slot-machine
	// rollback` still work over SSH. Reach it from elsewhere with a tunnel.
	apiAddr := fmt.Sprintf("127.0.0.1:%d", apiPort)
	apiLn, err := net.Listen("tcp", apiAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot listen on %s: %v\n", apiAddr, err)
		fmt.Fprintln(os.Stderr, "another slot-machine may already be running here")
		os.Exit(1)
	}

	// Say what the agent surface will and will not check, at the one moment an
	// operator is definitely reading the output. "agent auth: header" alone
	// invites the reading that slot-machine authenticates; it does not.
	switch cfg.AgentAuth {
	case "none":
		fmt.Printf("agent auth: none — every request is treated as %q. Local development only.\n",
			"local")
	default:
		fmt.Printf("agent auth: identity read from %s, set by the proxy in front\n", cfg.AgentAuthHeader)
		switch cfg.AgentAccess {
		case "allAuth":
			fmt.Println("agent access: every authenticated user")
		default:
			fmt.Printf("agent access: decided by the app at %s on its internal port\n",
				cfg.AgentAccessEndpoint)
		}
	}
	if cfg.Listen != "127.0.0.1" && cfg.Listen != "localhost" && cfg.Listen != "::1" {
		fmt.Printf("WARNING: listen is %q, so the identity header can be set by anything that "+
			"can reach this host. Only do this when the authenticating proxy is elsewhere and "+
			"the path to it is protected.\n", cfg.Listen)
	}
	reportAgentCredentials()

	st, err := store.Open(filepath.Join(*dataDir, "agent.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening agent store: %v\n", err)
		os.Exit(1)
	}

	mgr := agent.NewManager(st)

	// Reconcile the database with reality before accepting work: an agent
	// process that outlived the previous daemon is still holding the machine
	// slot and can still commit and deploy from it.
	if n, err := mgr.ReapOrphans(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not recover agent sessions: %v\n", err)
	} else if n > 0 {
		fmt.Printf("recovered %d interrupted agent session(s)\n", n)
	}

	// Find the Claude CLI in the background, installing it if there is none.
	//
	// In the background because installing means a download, and this used to
	// run inline — ahead of StartProxies and the auto-deploy — so on a machine
	// without Claude Code the application stayed unreachable for the length of
	// that download. The app is the thing this daemon exists to keep serving; it
	// must not wait on a tool that only the chat needs.
	//
	// Turns wait for this instead, via BinFunc: a chat turn can afford to block
	// on the agent's binary, a public port cannot.
	var agentBin string
	agentBinReady := make(chan struct{})
	go func() {
		defer close(agentBinReady)

		agentBin = agent.ResolveClaude(*dataDir)
		if agentBin == "" {
			var installErr error
			agentBin, installErr = agent.InstallClaude(*dataDir)
			if installErr != nil {
				fmt.Fprintf(os.Stderr, "warning: %v\n", installErr)
				fmt.Fprintln(os.Stderr, "set SLOT_MACHINE_AGENT_BIN to the claude binary path")
				return
			}
		}
		if v := agent.CLIVersion(agentBin); v != "" {
			fmt.Printf("agent binary: %s (%s)\n", agentBin, v)
		} else {
			fmt.Printf("agent binary: %s\n", agentBin)
		}
	}()

	// Reading agentBin after receiving on the closed channel is ordered by the
	// close; nothing else reads it.
	resolveAgentBin := func() string {
		<-agentBinReady
		return agentBin
	}

	// Declared before the agent service so the service can ask it which app is
	// live, and assigned below so the orchestrator can mount the service on the
	// public port. The cycle is only in the wiring: the closure runs when a
	// request arrives, long after both exist.
	var o *orchestrator.Orchestrator

	svc := agent.NewService(agent.Options{
		Store:          st,
		Manager:        mgr,
		BinFunc:        resolveAgentBin,
		WorkDir:        filepath.Join(*dataDir, orchestrator.MachineSlotName),
		DataDir:        *dataDir,
		ConfigPath:     *configPath,
		AuthMode:       cfg.AgentAuth,
		AuthHeader:     cfg.AgentAuthHeader,
		AccessMode:     cfg.AgentAccess,
		AccessEndpoint: cfg.AgentAccessEndpoint,
		LivePort: func() int {
			if o == nil {
				return 0
			}
			return o.LiveInternalPort()
		},
		AllowedTools:   cfg.AgentAllowedTools,
		DeniedCommands: cfg.AgentDeniedCommands,
		Model:          cfg.AgentModel,
		Timeout:        time.Duration(cfg.AgentTimeoutS) * time.Second,
		MachineBranch:  cfg.MachineBranch,
		HumanBranch:    cfg.HumanBranch,
		ChatTitle:      cfg.ChatTitle,
		ChatAccent:     cfg.ChatAccent,
		Env: func() []string {
			env := os.Environ()
			if cfg.EnvFile != "" {
				envPath := cfg.EnvFile
				if !filepath.IsAbs(envPath) {
					envPath = filepath.Join(absRepo, envPath)
				}
				if extra, err := config.LoadEnvFile(envPath); err == nil {
					env = append(env, extra...)
				}
			}
			return env
		},
	})

	o = orchestrator.New(orchestrator.Options{
		Config:    cfg,
		RepoDir:   absRepo,
		DataDir:   *dataDir,
		Intercept: svc,
	})

	o.WarnSharedDirs()

	// The agent's worktree. Created once and never rewritten by the daemon, so
	// it survives deploys and restarts with its dependencies and any
	// uncommitted work intact.
	if err := o.EnsureMachineSlot(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		fmt.Fprintln(os.Stderr, "the chat agent will not be able to work until this is resolved")
	} else {
		fmt.Printf("machine slot: %s (branch %s)\n", o.MachineDir(), cfg.MachineBranch)
	}

	// Bind the public port before deploying anything, so /chat is reachable even
	// if every deploy below fails.
	o.StartProxies()

	// Recover state from symlinks, or auto-deploy HEAD.
	o.RecoverState()
	if o.LiveCommit() == "" {
		commit, err := orchestrator.HeadCommit(absRepo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot determine HEAD: %v\n", err)
		} else {
			fmt.Printf("auto-deploying HEAD (%s)...\n", orchestrator.ShortHash(commit))
			resp, _ := o.Deploy(commit)
			if resp.Success {
				fmt.Printf("deployed %s to %s\n", orchestrator.ShortHash(resp.Commit), resp.Slot)
			} else {
				fmt.Fprintf(os.Stderr, "auto-deploy failed at %s: %s\n", resp.Stage, resp.Error)
			}
		}
	}

	apiSrv := &http.Server{Handler: o}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		fmt.Println("\nshutting down...")
		mgr.Stop()
		o.Shutdown()
		st.Close()
		apiSrv.Shutdown(context.Background())
	}()

	fmt.Printf("slot-machine listening on %s\n", apiAddr)
	if err := apiSrv.Serve(apiLn); err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}

// reportAgentCredentials says which credential the agent will use, because "the
// agent does nothing" is otherwise indistinguishable from "there is no token".
func reportAgentCredentials() {
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		fmt.Println("agent auth source: oauth token")
		return
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
			fmt.Println("agent auth source: credentials file")
			return
		}
	}
	fmt.Fprintln(os.Stderr, "warning: no Claude credentials found; "+
		"set CLAUDE_CODE_OAUTH_TOKEN or run `claude login`")
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
		c, err := orchestrator.HeadCommit(cwd)
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

	var dr orchestrator.DeployResponse
	json.NewDecoder(resp.Body).Decode(&dr)

	if dr.Success {
		fmt.Printf("deployed %s to %s\n", orchestrator.ShortHash(dr.Commit), dr.Slot)
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

	var rr orchestrator.RollbackResponse
	json.NewDecoder(resp.Body).Decode(&rr)

	if rr.Success {
		fmt.Printf("rolled back to %s (%s)\n", orchestrator.ShortHash(rr.Commit), rr.Slot)
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

	var sr orchestrator.StatusResponse
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

func readAPIPort() int {
	cwd, _ := os.Getwd()
	dir := cwd
	for {
		data, err := os.ReadFile(filepath.Join(dir, "slot-machine.json"))
		if err == nil {
			var cfg config.Config
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
