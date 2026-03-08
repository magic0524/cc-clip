# Codex CLI Clipboard Support Design

**Date:** 2026-03-08
**Branch:** `feat/codex-support`
**Status:** Design complete, pending implementation

## 1. Problem Definition

cc-clip bridges the local Mac clipboard to remote Linux servers via SSH for Claude Code's Ctrl+V image paste. It works by shimming `xclip`/`wl-paste` — external commands Claude Code calls to read the clipboard.

OpenAI Codex CLI uses the Rust `arboard` crate, which accesses the clipboard **in-process** via X11 protocol — no external commands. The xclip shim cannot intercept this.

**Error in remote SSH:**
```
Failed to paste image: clipboard unavailable: Unknown error while interacting with
the clipboard: X11 server connection timed out because it was unreachable
```

**Root cause:** Headless Linux has no X server. `$DISPLAY` is unset or points to nothing. arboard's X11 connection times out.

## 2. Solution: Xvfb + Go X11 Selection Owner

Run a virtual X server (Xvfb) on the remote. A new `cc-clip x11-bridge` daemon claims X11 CLIPBOARD ownership. When Codex pastes (arboard sends `SelectionRequest`), x11-bridge fetches the image on-demand from the existing cc-clip HTTP tunnel.

**Why this approach:**

| Rejected alternative | Reason |
|----------------------|--------|
| Xvfb + xclip polling | Stale data; polling frequency hard to tune |
| Minimal X11 proxy (no Xvfb) | X11 server protocol too complex to implement |
| LD_PRELOAD interception | Fragile; depends on arboard internals |
| Upstream Codex contribution | Uncertain timeline; not in our control |

## 3. Architecture

```
Remote Linux Server
+---------------------------------------------------+
|                                                   |
|  Xvfb :N (-screen 0 1x1x24 -nolisten tcp)        |
|    ^                                              |
|    | X11 protocol (Unix socket)                   |
|    |                                              |
|  cc-clip x11-bridge (daemon)                      |
|    +- connects to DISPLAY=:N                      |
|    +- creates invisible window                    |
|    +- claims CLIPBOARD selection ownership         |
|    +- event loop: waits for SelectionRequest       |
|    +- on request -> HTTP GET /clipboard/image     |
|                      |                            |
|              127.0.0.1:18339 (SSH tunnel)          |
|                                                   |
|  Codex CLI                                        |
|    +- arboard -> XConvertSelection(CLIPBOARD)     |
|         | receives PNG data <- x11-bridge          |
|                                                   |
|  Claude Code (unaffected, uses xclip shim)        |
|                                                   |
+---------------------------------------------------+
         | SSH RemoteForward :18339
+---------------------------------------------------+
|  Local Mac                                        |
|  cc-clip serve -> 127.0.0.1:18339                 |
|    +- pngpaste -> Mac clipboard image             |
+---------------------------------------------------+
```

## 4. New File Structure

```
internal/x11bridge/
  +-- bridge.go        # X11 connection, event loop, clipboard owner
  +-- atoms.go         # Atom definitions and cache
  +-- selection.go     # SelectionRequest handling, TARGETS response
  +-- incr.go          # INCR protocol for images >256KB
  +-- bridge_test.go   # Unit + integration tests
  +-- e2e_test.go      # End-to-end smoke test
internal/xvfb/
  +-- xvfb.go          # Xvfb start, stop, display discovery
  +-- xvfb_test.go
```

**Go dependency:** `github.com/jezek/xgb` (pure Go X11 binding, no CGo, cross-compile safe).

## 5. X11 Selection Owner Logic

### 5.1 Two-Round Request Flow

**Round 1 — TARGETS query** ("what formats are available?"):

```
arboard -> XConvertSelection(CLIPBOARD, TARGETS, ...)
x11-bridge:
  1. Probe cc-clip HTTP tunnel
  2. GET /clipboard/type -> {"type":"image","format":"png"}
  3. type=image -> respond: [TARGETS, TIMESTAMP, image/png]
  4. type!=image -> respond: Property=None (refuse)
```

**Round 2 — Image fetch** ("give me image/png"):

```
arboard -> XConvertSelection(CLIPBOARD, image/png, ...)
x11-bridge:
  1. GET /clipboard/image -> binary PNG data
  2. data <= 256KB -> ChangeProperty (direct write)
  3. data > 256KB -> INCR chunked transfer
  4. SendEvent(SelectionNotify) to notify completion
```

### 5.2 INCR Protocol (Required for Large Images)

```
x11-bridge                              arboard
----------                              ------
Write INCR marker + SendEvent
                                        Detect type=INCR, delete property
Detect PropertyDelete
Write chunk 1 (<=64KB)
                                        Read chunk, delete property
Detect PropertyDelete
Write chunk 2
... repeat ...
Write empty property (len=0)            Transfer complete
```

### 5.3 Key Implementation Details

- **Atom caching:** `InternAtom` called once per atom, cached for lifetime.
- **Timeout protection:** HTTP fetch failure -> return `Property=None`.
- **SelectionClear recovery:** If another program takes ownership (unlikely on Xvfb), bridge reclaims.
- **Chunk size:** `MaxRequestLength / 4`.
- **Token reading:** Read from `~/.cache/cc-clip/session.token` on each HTTP request (not cached in memory). Enables `--token-only` rotation without bridge restart.

## 6a. Xvfb Lifecycle Management

### Display Allocation

Uses `Xvfb -displayfd` for dynamic, conflict-free display assignment. No hardcoded `:99`.

### State Directory

`~/.cache/cc-clip/codex/` on the remote host:

| File | Content |
|------|---------|
| `display` | Display number written by Xvfb via `-displayfd` |
| `xvfb.pid` | Xvfb process ID |
| `xvfb.log` | Xvfb stderr output |
| `bridge.pid` | x11-bridge process ID |
| `bridge.log` | x11-bridge log output |

### Startup Flow

1. Check `Xvfb` exists in PATH; fail with install hint if missing.
2. If `xvfb.pid` + `display` exist, validate:
   - PID process is alive (`kill -0`)
   - `/tmp/.X11-unix/X<n>` socket exists
3. Validation passes -> reuse existing instance.
4. Validation fails -> clean stale files and restart:

```bash
mkdir -p ~/.cache/cc-clip/codex
rm -f ~/.cache/cc-clip/codex/display
nohup Xvfb -displayfd 1 -screen 0 1x1x24 -nolisten tcp \
  > ~/.cache/cc-clip/codex/display \
  2> ~/.cache/cc-clip/codex/xvfb.log \
  < /dev/null &
echo $! > ~/.cache/cc-clip/codex/xvfb.pid
for i in 1 2 3 4 5; do
  [ -s ~/.cache/cc-clip/codex/display ] && break
  sleep 0.2
done
cat ~/.cache/cc-clip/codex/display
```

### Lifecycle Rules

- Xvfb persists after `connect --codex` exits.
- Subsequent `connect --codex` reuses healthy instance.
- Only explicit uninstall stops it.
- No systemd service, no login auto-start in v1.

## 6b. `cc-clip connect --codex` Integration

### CLI Interface

User-facing:
- `cc-clip connect <host> --codex`
- `cc-clip setup <host> --codex`

Internal (remote):
- `cc-clip x11-bridge --display :N --port 18339`

### Flag Semantics

| Flag combination | Behavior |
|------------------|----------|
| `--codex` | Append Codex setup after Claude deploy (steps 7-10) |
| `--force --codex` | Force rebuild: restart Xvfb, restart bridge, rewrite DISPLAY marker |
| `--token-only --codex` | Only sync token; skip Xvfb/bridge/marker (bridge reads token from file) |

### Extended 10-Step Flow

Steps 1-6 remain unchanged (Claude path).

| Step | Description |
|------|-------------|
| 7 | **Codex preflight:** check `Xvfb` available, create `~/.cache/cc-clip/codex/` |
| 8 | **Start or reuse Xvfb:** validate health or `-displayfd` fresh start |
| 9 | **Start or reuse x11-bridge:** if binary was uploaded in step 4, unconditionally restart bridge |
| 10 | **Inject DISPLAY marker + verify + write deploy state** |

### Background Process Launch

Both Xvfb and x11-bridge use `nohup ... < /dev/null &` to survive SSH session exit.

```bash
display="$(cat ~/.cache/cc-clip/codex/display)"
nohup env DISPLAY=":$display" ~/.local/bin/cc-clip x11-bridge \
  --display ":$display" \
  --port 18339 \
  > ~/.cache/cc-clip/codex/bridge.log \
  2>&1 < /dev/null &
echo $! > ~/.cache/cc-clip/codex/bridge.pid
```

### Failure Strategy

- Steps 1-6 succeed -> Claude support is live.
- Steps 7-10 fail -> non-zero exit code. Output: "Claude shim is ready, but Codex support failed."
- Only roll back newly started Codex components; do not touch reused instances.

## 6c. DISPLAY Environment Variable Injection

### Approach

Dynamic file-based injection via shell rc marker block. Reads display number from `~/.cache/cc-clip/codex/display` at shell startup — not hardcoded.

### Marker Block

```sh
# >>> cc-clip Codex DISPLAY (do not edit) >>>
if [ -z "${DISPLAY:-}" ] && [ -r "$HOME/.cache/cc-clip/codex/display" ]; then
  _cc_clip_display="$(cat "$HOME/.cache/cc-clip/codex/display" 2>/dev/null)"
  case "$_cc_clip_display" in
    :[0-9]*) _cc_clip_num="${_cc_clip_display#:}" ;;
    [0-9]*)  _cc_clip_num="$_cc_clip_display" ;;
    *)       _cc_clip_num="" ;;
  esac
  if [ -n "$_cc_clip_num" ] && [ -S "/tmp/.X11-unix/X${_cc_clip_num}" ]; then
    export DISPLAY=":${_cc_clip_num}"
  fi
  unset _cc_clip_display _cc_clip_num
fi
# <<< cc-clip Codex DISPLAY (do not edit) <<<
```

### Design Rules

- Only injects when `$DISPLAY` is empty — user environment takes priority.
- Validates X socket existence before setting — prevents stale display.
- POSIX shell compatible (bash + zsh).
- Prepended to rc file top (same strategy as PATH marker, bypasses interactive guards).
- Separate from PATH marker — independent lifecycle.
- Idempotent: block content is static; `--force --codex` does not need to rewrite it.

## 6d. DeployState Extension

### Structure

```go
type DeployState struct {
    BinaryHash    string            `json:"binary_hash"`
    BinaryVersion string            `json:"binary_version"`
    ShimInstalled bool              `json:"shim_installed"`
    ShimTarget    string            `json:"shim_target"`
    PathFixed     bool              `json:"path_fixed"`
    Codex         *CodexDeployState `json:"codex,omitempty"`
}

type CodexDeployState struct {
    Enabled      bool   `json:"enabled"`
    Mode         string `json:"mode"`          // "x11-bridge"
    DisplayFixed bool   `json:"display_fixed"` // DISPLAY marker injected
}
```

### Principles

- **deploy.json records intent, filesystem records runtime state.**
- `Codex: nil` (absent from JSON) = "never configured" — backward compatible.
- No PID, display value, or log paths in deploy.json.

### Write Rules

| Action | deploy.json codex |
|--------|-------------------|
| `connect --codex` success | Write `{enabled: true, mode: "x11-bridge", display_fixed: true}` |
| `connect` without `--codex` | Do not modify existing codex block |
| `--token-only --codex` | Do not modify |
| `--force --codex` success | Rewrite codex block |
| Codex steps fail | Do not update codex block |
| `uninstall --codex` | Delete entire codex block |

### Diagnostic Semantics

| State | Meaning |
|-------|---------|
| `codex` absent | Never configured |
| `codex.enabled=true`, runtime unhealthy | Configured but drifted/broken |
| `codex.enabled=true`, runtime healthy | Working |

## 6e. Error Handling

### Strategy: Strict at deploy time, soft at runtime.

### A. `connect --codex` Errors (Must Block)

- Remote is not Linux -> error with message
- `Xvfb` not found -> error with install hint (`apt install xvfb` / `dnf install xorg-x11-server-Xvfb`)
- Xvfb start failure (empty display file, socket timeout) -> error, dump `xvfb.log` tail
- x11-bridge start failure -> error, dump `bridge.log` tail
- DISPLAY marker injection failure -> error
- Binary uploaded in step 4 + old bridge running -> unconditionally restart bridge

All Codex errors preserve Claude path (steps 1-6 already succeeded).

### B. Runtime Soft Failures (Per-Request, Bridge Stays Alive)

| Condition | Action |
|-----------|--------|
| Token file missing/unreadable | Return `Property=None`, log |
| Tunnel unreachable | Return `Property=None`, log |
| `/clipboard/type` returns non-image | TARGETS response omits image formats |
| `/clipboard/image` returns 204/empty | Return `Property=None`, log |
| Token invalid (401/403) | Return `Property=None`, log |
| INCR transfer interrupted | Abort transfer, log, continue event loop |

### C. Fatal Errors (Bridge Exits)

- Cannot connect to `$DISPLAY` at startup
- Atom initialization failure
- Cannot claim CLIPBOARD ownership
- X11 connection closed by server
- Unrecoverable X11 protocol error

### D. Concurrency

v1: Serial `SelectionRequest` processing. During active INCR transfer, new requests get `Property=None`. Single-user low-frequency paste scenario does not need concurrent state machine.

### E. Stale State

Auto-detected and auto-cleaned (idempotent):
- `xvfb.pid` exists but process dead
- `display` file exists but socket missing
- `bridge.pid` exists but process dead
- `display` content not a valid number

### F. Intentionally Not Fixed (v1)

User has existing `$DISPLAY` pointing to a broken X server. Our marker respects existing `$DISPLAY` (does not override). `doctor --host` should diagnose and suggest `unset DISPLAY`.

### G. Logging

- **Deploy time failure:** Print last 20 lines of relevant log file.
- **Runtime:** One-line structured entries: `token read failed`, `tunnel unreachable`, `clipboard no image`, `image fetch returned empty`, `incr transfer aborted`.

## 6f. Testing Strategy

### Layer 1: Unit Tests (No External Dependencies)

Always run, including CI without Xvfb.

| File | Tests |
|------|-------|
| `atoms_test.go` | Atom name constants |
| `xvfb_test.go` (unit) | Display file parsing, stale detection, socket path generation |
| `deploy_test.go` (extended) | CodexDeployState serialization, backward compat, `NeedsCodexSetup()` |
| `pathfix_test.go` (extended) | DISPLAY marker generation, independence from PATH marker, idempotency, selective uninstall |

### Layer 2: X11 Integration Tests (Need Xvfb)

Skip guard pattern:

```go
func requireXvfb(t *testing.T) string {
    t.Helper()
    if _, err := exec.LookPath("Xvfb"); err != nil {
        t.Skip("Xvfb not available, skipping X11 test")
    }
    // Start temp Xvfb with -displayfd, t.Cleanup() stops it
}
```

Each test gets its own Xvfb instance via `-displayfd`. Full isolation.

| Test | What It Validates |
|------|-------------------|
| `TestBridge_ClaimOwnership` | Connect to Xvfb, claim CLIPBOARD, verify via `GetSelectionOwner` |
| `TestBridge_TargetsResponse` | Mock HTTP returns image -> xclip TARGETS -> response contains `image/png` |
| `TestBridge_TargetsNoImage` | Mock HTTP returns text -> TARGETS -> no `image/png` |
| `TestBridge_ImageSmall` | Mock HTTP returns <256KB PNG -> xclip reads -> bytes match |
| `TestBridge_ImageLargeINCR` | Mock HTTP returns >256KB PNG -> xclip reads -> bytes match |
| `TestBridge_TunnelDown` | No HTTP server -> xclip request -> error (not hang) |
| `TestBridge_TokenInvalid` | Mock HTTP returns 401 -> xclip request -> error |
| `TestBridge_EmptyImage204` | Mock HTTP returns 204 -> xclip request -> error |
| `TestBridge_SelectionClearRecovery` | Another program takes ownership -> bridge reclaims |
| `TestXvfb_StartAndStop` | Start -> validate display/socket/PID -> stop -> cleanup verified |
| `TestXvfb_ReuseHealthy` | Start -> start again -> PID unchanged (reuse) |
| `TestXvfb_RecoverStale` | Start -> kill -> detect stale -> restart succeeds |

Uses `xclip` as requestor (same X11 selection protocol as arboard).

### Layer 3: End-to-End Smoke Test

```
TestE2E_FullPasteFlow:
  1. Start mock HTTP server (returns fixed PNG)
  2. Start temp Xvfb
  3. Start x11-bridge (mock HTTP + temp Xvfb)
  4. xclip -selection clipboard -t TARGETS -o -> verify contains image/png
  5. xclip -selection clipboard -t image/png -o -> get image
  6. Compare bytes with original PNG
  7. Cleanup
```

If this passes, real Codex (arboard) will also work — same X11 protocol.

### Layer 4: Not Automated

| Item | Reason | Verification |
|------|--------|-------------|
| Real SSH `connect --codex` | Needs remote server | Manual |
| Real Codex CLI paste | Needs Codex + OpenAI account | Manual |
| nohup process persistence | Needs SSH disconnect/reconnect | Manual |

### CI Configuration

```yaml
- name: Install X11 test dependencies
  run: sudo apt-get install -y xvfb xclip

- name: Run tests
  run: make test
```

No build tags needed. `requireXvfb(t)` auto-skips when Xvfb is not available.

### Manual Acceptance Checklist (Pre-Merge)

- [ ] `cc-clip connect <host> --codex` completes all 10 steps
- [ ] Remote Xvfb alive, display file correct
- [ ] Remote x11-bridge alive, bridge.log clean
- [ ] DISPLAY marker in remote .bashrc/.zshrc top
- [ ] New SSH shell: `echo $DISPLAY` shows correct `:N`
- [ ] Codex CLI Ctrl+V pastes image successfully
- [ ] `cc-clip connect <host>` (no --codex) does not affect Codex components
- [ ] `cc-clip connect <host> --token-only --codex` only syncs token
- [ ] `cc-clip connect <host> --force --codex` rebuilds all Codex components
- [ ] SSH disconnect + reconnect: Xvfb and x11-bridge still alive
- [ ] Claude Code Ctrl+V still works (regression)

## 6g. Uninstall and Cleanup

### CLI Entry Points

- `cc-clip uninstall --host <host> --codex` — Local orchestration via SSH.
- `cc-clip uninstall --codex` — On-machine cleanup (used internally by remote binary).
- Existing `cc-clip uninstall` (without `--codex`) — unchanged, Claude shim only.

### Cleanup Scope

**Cleaned:**
- Stop x11-bridge process
- Stop Xvfb process
- Delete `~/.cache/cc-clip/codex/` (all runtime files)
- Remove DISPLAY marker from shell rc
- Remove `codex` block from `deploy.json`

**Not touched:**
- cc-clip binary
- Token file
- Claude xclip/wl-paste shim
- PATH marker

### Cleanup Order

1. Stop x11-bridge (dependent)
2. Stop Xvfb (dependency)
3. Delete `~/.cache/cc-clip/codex/`
4. Remove DISPLAY marker from rc
5. Update `deploy.json` (remove codex block)
6. Verify and output result

### Process Stop Strategy

```bash
# Example: stop bridge safely
pid=$(cat ~/.cache/cc-clip/codex/bridge.pid 2>/dev/null) && \
  [ -n "$pid" ] && \
  ps -p "$pid" -o args= 2>/dev/null | grep -q 'cc-clip x11-bridge' && \
  kill "$pid" 2>/dev/null && \
  sleep 0.5 && \
  kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null; \
  true
```

- PID + command-line verification before kill (no blind PID kill).
- TERM first, KILL if still alive after 0.5s.
- PID exists but command mismatch -> do not kill, report to user, return non-zero.

### Idempotency

Every "not found" condition is treated as "already cleaned." Repeated runs succeed with exit 0.

### Binary Missing Fallback

If remote `cc-clip` binary is missing, fall back to shell-based cleanup via SSH:
- Delete DISPLAY marker (sed)
- Delete `~/.cache/cc-clip/codex/`
- Update `deploy.json` (remove codex key)
- Kill processes only if command-line verification passes

### Return Codes

- Target state reached -> 0
- "Already clean" -> 0
- Residual process unsafe to stop / marker cannot be removed / state update failed -> non-zero

## Appendix: Files That Need Coordinated Changes

Adding Codex support touches:

| Change | Files |
|--------|-------|
| New x11-bridge subcommand | `cmd/cc-clip/main.go` |
| X11 selection owner | `internal/x11bridge/bridge.go`, `atoms.go`, `selection.go`, `incr.go` |
| Xvfb management | `internal/xvfb/xvfb.go` |
| Connect --codex steps | `cmd/cc-clip/main.go:runConnect()` |
| DISPLAY marker | `internal/shim/pathfix.go` (extend) |
| DeployState | `internal/shim/deploy.go` (extend) |
| Uninstall --codex | `cmd/cc-clip/main.go:cmdUninstall()` |
| Go dependency | `go.mod` (add `github.com/jezek/xgb`) |
| Tests | `internal/x11bridge/*_test.go`, `internal/xvfb/*_test.go`, extended existing tests |
| CI | `.github/workflows/` (add xvfb + xclip install step) |
