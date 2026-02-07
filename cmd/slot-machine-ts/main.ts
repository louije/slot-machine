#!/usr/bin/env bun
// slot-machine-ts — blue-green deploys on a single machine.
//
// A TypeScript/Bun implementation of the slot-machine spec. Passes the same
// 17 tests as the Go version, proving the spec is language-agnostic.
//
// Build:
//   bun build --compile cmd/slot-machine-ts/main.ts --outfile slot-machine-ts
//
// Run tests:
//   ORCHESTRATOR_BIN=$(pwd)/slot-machine-ts go test -v -count=1 ./spec/

import {
  spawn as nodeSpawn,
  execSync,
  type ChildProcess,
} from "node:child_process";
import {
  readFileSync,
  writeFileSync,
  existsSync,
  mkdirSync,
  statSync,
  openSync,
  rmSync,
} from "node:fs";
import { join, resolve } from "node:path";

const specVersion = "1"; // spec/VERSION

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface Config {
  setup_command?: string;
  start_command: string;
  port: number;
  internal_port: number;
  health_endpoint: string;
  health_timeout_ms: number;
  drain_timeout_ms: number;
  env_file?: string;
  api_port?: number;
}

interface Slot {
  commit: string;
  dir: string;
  process: ChildProcess;
  alive: boolean;
  done: Promise<void>;
}

// ---------------------------------------------------------------------------
// Orchestrator state
// ---------------------------------------------------------------------------

let config: Config;
let repoDir: string;
let dataDir: string;
let deploying = false;
let liveSlot = "";
let prevSlot = "";
const slots: Record<string, Slot> = {};
let lastDeploy: Date | null = null;
let server: ReturnType<typeof Bun.serve> | null = null;

// ---------------------------------------------------------------------------
// Env file loading
// ---------------------------------------------------------------------------

function loadEnvFile(path: string): Record<string, string> {
  const env: Record<string, string> = {};
  try {
    const content = readFileSync(path, "utf-8");
    for (const line of content.split("\n")) {
      const trimmed = line.trim();
      if (!trimmed || trimmed.startsWith("#")) continue;
      const eq = trimmed.indexOf("=");
      if (eq > 0) {
        env[trimmed.slice(0, eq)] = trimmed.slice(eq + 1);
      }
    }
  } catch {}
  return env;
}

function buildEnv(): Record<string, string> {
  const env: Record<string, string> = { ...process.env } as Record<
    string,
    string
  >;
  if (config.env_file) {
    const extra = loadEnvFile(config.env_file);
    Object.assign(env, extra);
  }
  // Contract ports override env_file.
  env.PORT = String(config.port);
  env.INTERNAL_PORT = String(config.internal_port);
  return env;
}

// ---------------------------------------------------------------------------
// Git worktree
// ---------------------------------------------------------------------------

function git(args: string[], cwd?: string): string {
  return execSync(["git", ...args].join(" "), {
    cwd,
    encoding: "utf-8",
    stdio: ["pipe", "pipe", "pipe"],
  }).trim();
}

function prepareSlot(slotDir: string, commit: string): void {
  if (existsSync(join(slotDir, ".git"))) {
    // Existing worktree — just checkout the new commit.
    git(["checkout", "--force", "--detach", commit], slotDir);
    return;
  }

  // Remove leftovers and create fresh worktree.
  rmSync(slotDir, { recursive: true, force: true });
  git(["worktree", "prune"], repoDir);
  git(
    ["worktree", "add", "--detach", slotDir, commit],
    repoDir
  );
}

// ---------------------------------------------------------------------------
// Process management
// ---------------------------------------------------------------------------

function startProcess(dir: string, commit: string): Slot {
  const env = buildEnv();
  const logPath = join(dataDir, `${dir.split("/").pop()}.log`);
  const logFd = openSync(logPath, "a");

  const proc = nodeSpawn("/bin/sh", ["-c", config.start_command], {
    cwd: dir,
    env,
    detached: true, // new process group
    stdio: ["ignore", logFd, logFd],
  });

  let resolveDone!: () => void;
  const done = new Promise<void>((r) => {
    resolveDone = r;
  });

  const slot: Slot = { commit, dir, process: proc, alive: true, done };

  proc.on("exit", () => {
    slot.alive = false;
    resolveDone();
  });

  return slot;
}

function runSetup(dir: string): void {
  if (!config.setup_command) return;
  execSync(config.setup_command, {
    cwd: dir,
    env: buildEnv(),
    stdio: "inherit",
  });
}

async function drain(slot: Slot): Promise<void> {
  if (!slot.process.pid) return;

  try {
    process.kill(-slot.process.pid, "SIGTERM");
  } catch {}

  const timeout = new Promise<"timeout">((r) =>
    setTimeout(() => r("timeout"), config.drain_timeout_ms)
  );
  const result = await Promise.race([
    slot.done.then(() => "done" as const),
    timeout,
  ]);

  if (result === "timeout") {
    try {
      process.kill(-slot.process.pid, "SIGKILL");
    } catch {}
    await slot.done;
  }
}

async function drainAll(): Promise<void> {
  const all = Object.values(slots);
  await Promise.all(all.map((s) => drain(s)));
}

async function healthCheck(slot: Slot): Promise<boolean> {
  const deadline = Date.now() + config.health_timeout_ms;
  const url = `http://127.0.0.1:${config.internal_port}${config.health_endpoint}`;

  while (Date.now() < deadline) {
    if (!slot.alive) return false;

    try {
      const resp = await fetch(url, { signal: AbortSignal.timeout(500) });
      if (resp.status === 200) {
        await resp.text(); // drain body
        return true;
      }
    } catch {}

    await new Promise((r) => setTimeout(r, 200));
  }
  return false;
}

// ---------------------------------------------------------------------------
// Deploy logic
// ---------------------------------------------------------------------------

interface DeployResponse {
  success: boolean;
  slot: string;
  commit: string;
  previous_commit: string;
  error?: string;
}

async function doDeploy(commit: string): Promise<[DeployResponse, number]> {
  if (deploying) {
    return [
      { success: false, slot: "", commit: "", previous_commit: "", error: "deploy in progress" },
      409,
    ];
  }
  deploying = true;
  const currentLive = liveSlot;
  const liveSlotObj = currentLive ? slots[currentLive] : null;

  try {
    const inactive = currentLive === "a" ? "b" : "a";
    const slotDir = join(dataDir, `slot-${inactive}`);

    prepareSlot(slotDir, commit);
    runSetup(slotDir);

    if (liveSlotObj) {
      await drain(liveSlotObj);
    }

    const newSlot = startProcess(slotDir, commit);
    const healthy = await healthCheck(newSlot);

    if (healthy) {
      const prevCommit =
        currentLive && slots[currentLive]
          ? slots[currentLive].commit
          : "";
      prevSlot = currentLive;
      liveSlot = inactive;
      slots[inactive] = newSlot;
      lastDeploy = new Date();

      return [
        {
          success: true,
          slot: inactive,
          commit,
          previous_commit: prevCommit,
        },
        200,
      ];
    }

    // Unhealthy — kill the new process.
    try {
      process.kill(-newSlot.process.pid!, "SIGKILL");
    } catch {}
    await newSlot.done;

    return [
      { success: false, slot: "", commit: "", previous_commit: "" },
      200,
    ];
  } finally {
    deploying = false;
  }
}

// ---------------------------------------------------------------------------
// Rollback logic
// ---------------------------------------------------------------------------

interface RollbackResponse {
  success: boolean;
  slot: string;
  commit: string;
  error?: string;
}

async function doRollback(): Promise<[RollbackResponse, number]> {
  if (deploying) {
    return [
      { success: false, slot: "", commit: "", error: "deploy in progress" },
      409,
    ];
  }
  if (!prevSlot) {
    return [
      { success: false, slot: "", commit: "", error: "no previous slot" },
      400,
    ];
  }
  deploying = true;

  const prevSlotName = prevSlot;
  const prevSlotObj = slots[prevSlotName];
  const liveSlotName = liveSlot;
  const liveSlotObj = slots[liveSlotName];

  try {
    if (liveSlotObj) {
      await drain(liveSlotObj);
    }

    const newSlot = startProcess(prevSlotObj.dir, prevSlotObj.commit);
    const healthy = await healthCheck(newSlot);

    if (healthy) {
      liveSlot = prevSlotName;
      prevSlot = liveSlotName;
      slots[prevSlotName] = newSlot;
      lastDeploy = new Date();

      return [
        { success: true, slot: prevSlotName, commit: prevSlotObj.commit },
        200,
      ];
    }

    try {
      process.kill(-newSlot.process.pid!, "SIGKILL");
    } catch {}
    await newSlot.done;
    return [
      { success: false, slot: "", commit: "", error: "health check failed" },
      500,
    ];
  } finally {
    deploying = false;
  }
}

// ---------------------------------------------------------------------------
// HTTP API
// ---------------------------------------------------------------------------

function handleStatus(): Response {
  const resp: Record<string, unknown> = {
    live_slot: liveSlot,
    live_commit: "",
    previous_slot: prevSlot,
    previous_commit: "",
    last_deploy_time: "",
    healthy: false,
  };

  if (liveSlot && slots[liveSlot]) {
    resp.live_commit = slots[liveSlot].commit;
    resp.healthy = slots[liveSlot].alive;
  }
  if (prevSlot && slots[prevSlot]) {
    resp.previous_commit = slots[prevSlot].commit;
  }
  if (lastDeploy) {
    resp.last_deploy_time = lastDeploy.toISOString();
  }

  return Response.json(resp);
}

async function handleRequest(req: Request): Promise<Response> {
  const url = new URL(req.url);

  if (req.method === "GET" && url.pathname === "/") {
    return Response.json({ status: "ok" });
  }

  if (req.method === "POST" && url.pathname === "/deploy") {
    const body = (await req.json()) as { commit?: string };
    if (!body.commit) {
      return Response.json(
        { success: false, error: "missing commit" },
        { status: 400 }
      );
    }
    const [resp, code] = await doDeploy(body.commit);
    return Response.json(resp, { status: code });
  }

  if (req.method === "POST" && url.pathname === "/rollback") {
    const [resp, code] = await doRollback();
    return Response.json(resp, { status: code });
  }

  if (req.method === "GET" && url.pathname === "/status") {
    return handleStatus();
  }

  return new Response("Not Found", { status: 404 });
}

// ---------------------------------------------------------------------------
// CLI: init
// ---------------------------------------------------------------------------

function cmdInit(): void {
  const cwd = process.cwd();

  const cfg: Record<string, unknown> = {
    port: 3000,
    internal_port: 3000,
    health_endpoint: "/healthz",
    health_timeout_ms: 10000,
    drain_timeout_ms: 5000,
    api_port: 9100,
  };

  // Detect project type.
  if (existsSync(join(cwd, "bun.lock"))) {
    cfg.setup_command = "bun install --frozen-lockfile";
    cfg.start_command = readStartScript(cwd, "bun");
  } else if (existsSync(join(cwd, "package-lock.json"))) {
    cfg.setup_command = "npm ci";
    cfg.start_command = readStartScript(cwd, "node");
  } else if (existsSync(join(cwd, "requirements.txt"))) {
    cfg.setup_command = "pip install -r requirements.txt";
    cfg.start_command = "python app.py";
  } else if (existsSync(join(cwd, "Gemfile.lock"))) {
    cfg.setup_command = "bundle install";
    cfg.start_command = "bundle exec ruby app.rb";
  }

  if (existsSync(join(cwd, ".env"))) {
    cfg.env_file = ".env";
  }

  const cfgPath = join(cwd, "slot-machine.json");
  writeFileSync(cfgPath, JSON.stringify(cfg, null, "  ") + "\n");
  console.log(`wrote ${cfgPath}`);

  // Append .slot-machine to .gitignore.
  const gitignorePath = join(cwd, ".gitignore");
  if (!gitignoreContains(gitignorePath, ".slot-machine")) {
    let prefix = "";
    if (existsSync(gitignorePath)) {
      const content = readFileSync(gitignorePath, "utf-8");
      if (content.length > 0 && !content.endsWith("\n")) {
        prefix = "\n";
      }
    }
    writeFileSync(gitignorePath, prefix + ".slot-machine\n", { flag: "a" });
    console.log("added .slot-machine to .gitignore");
  }
}

function readStartScript(dir: string, runtime: string): string {
  try {
    const pkg = JSON.parse(readFileSync(join(dir, "package.json"), "utf-8"));
    if (pkg.scripts?.start) return pkg.scripts.start;
    if (pkg.main) return `${runtime} ${pkg.main}`;
  } catch {}
  return `${runtime} index.js`;
}

function gitignoreContains(path: string, entry: string): boolean {
  try {
    const content = readFileSync(path, "utf-8");
    return content.split("\n").some((line) => line.trim() === entry);
  } catch {
    return false;
  }
}

// ---------------------------------------------------------------------------
// CLI: start
// ---------------------------------------------------------------------------

function cmdStart(args: string[]): void {
  let configPath = "";
  let repo = "";
  let data = "";
  let port = 0;

  // Simple flag parsing.
  for (let i = 0; i < args.length; i++) {
    switch (args[i]) {
      case "--config":
        configPath = args[++i];
        break;
      case "--repo":
        repo = args[++i];
        break;
      case "--data":
        data = args[++i];
        break;
      case "--port":
        port = parseInt(args[++i]);
        break;
      case "--no-proxy":
        break; // accepted but unused
    }
  }

  const cwd = process.cwd();
  if (!configPath) configPath = join(cwd, "slot-machine.json");
  if (!repo) repo = cwd;
  if (!data) data = join(cwd, ".slot-machine");

  // Load config.
  let cfgData: string;
  try {
    cfgData = readFileSync(configPath, "utf-8");
  } catch {
    process.stderr.write(`error: cannot read ${configPath}\n`);
    process.stderr.write("run 'slot-machine init' to create it\n");
    process.exit(1);
  }
  config = JSON.parse(cfgData);

  // Resolve API port: flag > config > 9100.
  let apiPort = 9100;
  if (config.api_port) apiPort = config.api_port;
  if (port) apiPort = port;

  repoDir = resolve(repo);
  dataDir = data;
  mkdirSync(dataDir, { recursive: true });

  // Drain managed processes on SIGTERM/SIGINT.
  const shutdown = async () => {
    console.log("\nshutting down...");
    await drainAll();
    server?.stop();
    process.exit(0);
  };
  process.on("SIGTERM", shutdown);
  process.on("SIGINT", shutdown);

  console.log(`slot-machine listening on :${apiPort}`);
  server = Bun.serve({
    port: apiPort,
    fetch: handleRequest,
  });
}

// ---------------------------------------------------------------------------
// CLI: deploy
// ---------------------------------------------------------------------------

async function cmdDeploy(args: string[]): Promise<void> {
  let commit = args[0] || "";

  if (!commit) {
    try {
      commit = execSync("git rev-parse HEAD", { encoding: "utf-8" }).trim();
    } catch {
      process.stderr.write("error: cannot determine HEAD commit\n");
      process.exit(1);
    }
  }

  const port = readAPIPort();
  let resp: Response;
  try {
    resp = await fetch(`http://127.0.0.1:${port}/deploy`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ commit }),
    });
  } catch {
    process.stderr.write("error: cannot reach slot-machine daemon\n");
    process.exit(1);
  }

  const dr = (await resp.json()) as DeployResponse;
  if (dr.success) {
    const short = dr.commit.length > 8 ? dr.commit.slice(0, 8) : dr.commit;
    console.log(`deployed ${short} to slot ${dr.slot}`);
  } else {
    process.stderr.write(`deploy failed: ${dr.error}\n`);
    process.exit(1);
  }
}

// ---------------------------------------------------------------------------
// CLI: rollback
// ---------------------------------------------------------------------------

async function cmdRollback(): Promise<void> {
  const port = readAPIPort();
  let resp: Response;
  try {
    resp = await fetch(`http://127.0.0.1:${port}/rollback`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
    });
  } catch {
    process.stderr.write("error: cannot reach slot-machine daemon\n");
    process.exit(1);
  }

  const rr = (await resp.json()) as RollbackResponse;
  if (rr.success) {
    const short = rr.commit.length > 8 ? rr.commit.slice(0, 8) : rr.commit;
    console.log(`rolled back to ${short} (slot ${rr.slot})`);
  } else {
    process.stderr.write(`rollback failed: ${rr.error}\n`);
    process.exit(1);
  }
}

// ---------------------------------------------------------------------------
// CLI: status
// ---------------------------------------------------------------------------

async function cmdStatus(): Promise<void> {
  const port = readAPIPort();
  let resp: Response;
  try {
    resp = await fetch(`http://127.0.0.1:${port}/status`);
  } catch {
    process.stderr.write("error: cannot reach slot-machine daemon\n");
    process.exit(1);
  }

  const sr = (await resp.json()) as {
    live_slot: string;
    live_commit: string;
    previous_slot: string;
    previous_commit: string;
    last_deploy_time: string;
    healthy: boolean;
  };

  const healthy = sr.healthy ? "yes" : "no";
  console.log(
    `live:     slot ${sr.live_slot}  ${sr.live_commit}  healthy=${healthy}`
  );
  if (sr.previous_slot) {
    console.log(
      `previous: slot ${sr.previous_slot}  ${sr.previous_commit}`
    );
  }
  if (sr.last_deploy_time) {
    console.log(`last deploy: ${sr.last_deploy_time}`);
  }
}

function readAPIPort(): number {
  const cwd = process.cwd();
  try {
    const data = readFileSync(join(cwd, "slot-machine.json"), "utf-8");
    const cfg = JSON.parse(data);
    return cfg.api_port || 9100;
  } catch {
    process.stderr.write(
      "error: cannot read slot-machine.json in current directory\n"
    );
    process.exit(1);
  }
}

// ---------------------------------------------------------------------------
// Main — subcommand routing
// ---------------------------------------------------------------------------

// Handle both `bun main.ts <cmd>` and compiled `./slot-machine-ts <cmd>`.
// In both cases argv[1] is a path (starts with / or .), not a subcommand.
const isPath = process.argv[1]?.startsWith("/") || process.argv[1]?.startsWith(".");
const cliArgs = process.argv.slice(isPath ? 2 : 1);
const subcommand = cliArgs[0];

if (!subcommand) {
  process.stderr.write("usage: slot-machine <command> [args]\n");
  process.stderr.write("\ncommands:\n");
  process.stderr.write("  init       scaffold slot-machine.json\n");
  process.stderr.write("  start      start the daemon\n");
  process.stderr.write("  deploy     deploy a commit\n");
  process.stderr.write("  rollback   rollback to previous\n");
  process.stderr.write("  status     show current status\n");
  process.stderr.write("  version    print version info\n");
  process.exit(1);
}

switch (subcommand) {
  case "init":
    cmdInit();
    break;
  case "start":
    cmdStart(cliArgs.slice(1));
    break;
  case "deploy":
    await cmdDeploy(cliArgs.slice(1));
    break;
  case "rollback":
    await cmdRollback();
    break;
  case "status":
    await cmdStatus();
    break;
  case "version":
    console.log(`slot-machine (ts/bun) spec/${specVersion}`);
    break;
  default:
    process.stderr.write(`unknown command: ${subcommand}\n`);
    process.exit(1);
}
