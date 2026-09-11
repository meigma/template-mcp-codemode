# mcp-devproxy

`mcp-devproxy` is a hot-reloading development proxy for MCP servers. The
client (Claude Code) connects to the proxy once and keeps that session for
the whole dev loop; the proxy watches the source tree, rebuilds the server on
change, swaps the child process, and re-advertises its tools via
`notifications/tools/list_changed` — no reconnect, no lost conversation.

It exists because developing an MCP server *with* an LLM is otherwise awkward:
the client spawns the server as a stdio subprocess and reads its tool list at
session start, so every code change normally requires killing the
conversation, rebuilding, and reconnecting.

The proxy is dev tooling: it lives in a nested Go module
(`github.com/meigma/template-mcp/tools/proxy`) so its dependencies never leak
into the template's `go.mod`, and it is deliberately excluded from releases.

## Quick start

Inside this template there is nothing to set up. The repository's checked-in
`.mcp.json` points Claude Code at a wrapper that builds the proxy through
Moon's cached `proxy:build` task and then execs it:

```json
{
  "mcpServers": {
    "dev": {
      "command": "sh",
      "args": [
        "-c",
        "moon run proxy:build >&2 && exec tools/proxy/bin/mcp-devproxy"
      ]
    }
  }
}
```

Start `claude` in the repository root, approve the project-scoped `dev`
server on first use, and edit the server source — the proxy rebuilds and
hot-swaps the server on every save. Two details of the wrapper are
load-bearing:

- The `>&2` redirect: stdout is the JSON-RPC channel on this hop, so the
  build step's output must go to stderr.
- Building through `proxy:build` rather than a one-time manual build: the
  task declares its inputs and outputs, so Moon skips it when nothing
  changed (warm starts are near-instant) and the proxy binary can never be
  missing or stale.

To run the proxy with explicit flags — for another repository layout, or
after renaming the template's binary — the child command after `--` is
re-run for every reload cycle with `{{artifact}}` replaced by that cycle's
freshly built binary. The full CLI shape:

```sh
mcp-devproxy \
  --build "go build -o {{artifact}} ./cmd/template-mcp" \
  --watch cmd --watch internal \
  [--debounce 300ms] [--quiesce 5s] [--terminate 1s] \
  -- {{artifact}} stdio
```

**Zero config inside this template:** every flag has a working default for
this repository's layout, so a bare `mcp-devproxy` (empty `args`) builds and
serves `./cmd/template-mcp` over stdio. Each defaulted value is announced on
stderr. The defaults live in one isolated file
(`internal/cli/defaults.go`) so extracting the proxy to a standalone
repository stays clean.

## Flags and environment

Every flag is also settable through an `MCP_DEVPROXY_*` environment variable;
flags take precedence over the environment, which takes precedence over
defaults.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `--build` | `MCP_DEVPROXY_BUILD` | `go build -o {{artifact}} ./cmd/template-mcp` * | Build command template. Split on whitespace — no shell, so quoting and arguments containing spaces are not supported. Must reference `{{artifact}}`. |
| `--watch` | `MCP_DEVPROXY_WATCH` | `cmd`, `internal` * | Directory to watch recursively for source changes. Repeatable; the environment form is a whitespace-separated list. |
| `--dir` | `MCP_DEVPROXY_DIR` | current directory | Working directory for the build command. |
| `--debounce` | `MCP_DEVPROXY_DEBOUNCE` | `300ms` | How long source-change bursts are coalesced before a rebuild starts. |
| `--quiesce` | `MCP_DEVPROXY_QUIESCE` | `5s` | How long a swap waits for in-flight tool calls on the old child to drain. |
| `--terminate` | `MCP_DEVPROXY_TERMINATE` | `1s` | How long each child shutdown escalation step (stdin close, SIGTERM, SIGKILL) waits. |
| `--verbose` | `MCP_DEVPROXY_VERBOSE` | `false` | Debug logging on stderr, including build output. |

\* Template-layout zero-config default, applied only when the flag is unset.

The child command is positional argv after `--` (default:
`{{artifact}} stdio`) and must reference `{{artifact}}` — the rebuilt
binary's path changes every cycle, so a child command that ignores it would
run a stale binary forever.

## How it works

The reload lifecycle is `SERVING → BUILDING → STARTING → SWAPPING → SERVING`,
with every failure edge returning to `SERVING` on the old child:

1. A debounced source change triggers a build into a unique per-cycle
   artifact path (never overwriting the running child's binary in place).
2. The new child is spawned, initialized, and health-gated: its tools are
   listed under a timeout and every definition validated. The old child keeps
   serving the whole time.
3. The proxy quiesces (new calls buffer, bounded and with per-call timeouts),
   waits up to the quiesce grace for in-flight calls, swaps the router to the
   new child, and closes the old one.
4. The old and new tool sets are diffed by a canonical fingerprint of the
   full wire definition; removed tools are unregistered and added or changed
   tools re-registered, which emits one coalesced `tools/list_changed`. An
   identical tool set emits nothing.
5. Buffered calls drain to the new child only if their tool's definition is
   unchanged; otherwise they get the stale-reload error below.

Cold start serves the client immediately with an empty tool set, then runs
the first build cycle; the first healthy child triggers a normal reconcile
and `list_changed`. A broken first build never blocks the session.

Failure handling, condensed:

| Failure | Behavior |
|---|---|
| Build fails | Keep the old child; log the compile output; stay `SERVING`. |
| New child fails init or health gate | Kill it; keep the old child. |
| First build/child fails (no old child yet) | Serve the empty tool set; retry with backoff. |
| Child crashes while serving | Restart the last good artifact (build-free) with exponential backoff; the session survives. |
| Call arrives mid-swap | Buffered; drained to the new child only if the tool's definition is unchanged. |
| Client exits or the proxy is signaled | Cancel any in-flight cycle, close every child, exit cleanly — no orphans. |

**Stale-reload errors.** A call issued against a tool the reload changed or
removed — buffered mid-swap, or sent by a session that has not re-listed
since the swap — is answered with a tool *result* (not a protocol error) the
LLM can read and self-correct from:

> tool "name" changed by dev reload; list refreshes next turn

The gate opens the moment the session re-lists, which Claude Code does on
`list_changed`. A non-idempotent call issued against old semantics is never
silently executed on new code.

Tool changes do not propagate mid-turn: reloads land whenever a build
finishes, and the client observes the new tool set at its next turn boundary.

## v1 fidelity gaps

The proxy forwards tools only. A child that relies on the following will see
differences from running directly, and every gap is logged loudly on stderr:

- **Sampling and elicitation** — the child gets an error; the proxy's
  upstream client has no handlers for them.
- **Roots** — `roots/list` is rejected with a method-not-found error.
- **Prompts and resources** — they appear *empty* to the client (the
  downstream capability envelope advertises them so a later version can
  forward them without a reconnect). The health gate logs a prominent warning
  when a child actually advertises prompts or resources.
- **Progress** — progress tokens are stripped from forwarded calls;
  cancellation still propagates.
- **Instructions** — the child's `instructions` are not forwarded (the
  downstream session initializes before the first child exists).

Child MCP `logging` is forwarded: the client's last `logging/setLevel` is
replayed to each new child, and child log notifications flow back downstream.

## Observability

- All proxy logging goes to **stderr**. stdout is the JSON-RPC protocol
  channel on both hops; nothing else may write to it.
- Each child's stderr is passed through to the proxy's stderr — the developer
  sees their server's logs as if it ran directly. The child also inherits the
  proxy's environment, as a direct run would.
- `--verbose` enables debug logging, including each cycle's build output.

## Manual acceptance procedure

The automated suites prove the proxy against a real MCP client and real child
processes, but the load-bearing client behavior — Claude Code re-fetching and
*applying* tool lists on `list_changed` — can only be verified against Claude
Code itself. Run this procedure inside this repository whenever Claude Code's
major version changes, and record the results in the table below.

### Setup

1. The checked-in `.mcp.json` already builds and launches the proxy. To watch
   the reload cycle, temporarily append a stderr redirect to its wrapper
   command:

   ```json
   {
     "mcpServers": {
       "dev": {
         "command": "sh",
         "args": [
           "-c",
           "moon run proxy:build >&2 && exec tools/proxy/bin/mcp-devproxy 2>>/tmp/mcp-devproxy.log"
         ]
       }
     }
   }
   ```

2. Start a tmux session with two panes from the repository root: one running
   `claude` (the conversation under test), one for editing source and tailing
   `/tmp/mcp-devproxy.log`.

### Scenarios

**(a) Added tool.** In the live conversation, confirm an existing tool (for
example `random_int`) is callable through the proxy. In the other pane, add a
new tool to `internal/mcpserver` that returns an unguessable secret string,
and save. Wait for the rebuild in the log, then — next turn — ask Claude to
call the new tool by name. **Pass:** Claude calls it and reports the secret,
with no reconnect. (This re-validates the 2026-06-09 bare-server result
through the full proxy.)

**(b) Schema-only change to a same-named tool.** Change only the schema of an
existing tool — rename one of `random_int`'s parameters, or add a new
required parameter — and save. Next turn, ask Claude to call that tool.
Record which outcome occurs:

- Claude applies the refreshed schema and the call succeeds with new-shape
  arguments; or
- Claude sends old-shape arguments — before it re-lists, the proxy's stale
  gate answers with the friendly stale-reload error; after it re-lists, the
  child's own validation rejects the stale arguments.

This scenario settles the open question of whether Claude Code refreshes the
cached definition of a same-named tool; until it is settled, only the second
outcome's behavior is guaranteed.

**(c) Cold start, pre-first-turn `list_changed`.** End the Claude Code
session and start a fresh one (which spawns the proxy). Begin a conversation
immediately. **Pass:** the session starts instantly (the tool set may be
empty on the very first turn), and the first build's tools are present and
callable by the first or second turn without a reconnect.

### Results

| Date | Claude Code version | (a) added tool | (b) schema-only change | (c) cold start | Notes |
|---|---|---|---|---|---|
| — | — | — | — | — | Not yet run through the proxy. |

### Empirical notes

- **2026-06-09, Claude Code 2.1.170 (bare server, pre-proxy):** Claude Code
  honors `tools/list_changed` on a live session — it re-fetched the tool list
  and called a newly added tool (returning an unguessable secret) the next
  turn, with no reconnect. This is the design's load-bearing fact.
- **2026-06-10, integration suite:** each child accepts a
  fresh, proxy-identity `initialize` — nothing replays the downstream
  client's init params. The handshake, logging-level replay, and health gate
  all succeed under the proxy's own identity (`TestIntegrationColdStart` in
  `internal/cli/integration_test.go`).

## Development

```sh
moon run proxy:check                 # format, lint, build, test
go test -short ./...                 # skips the slow E2E test
```

The E2E test (`internal/cli/e2e_test.go`) runs a real `go build` and real
child processes; it is guarded by `testing.Short()` and builds offline by
construction (enforced with `GOPROXY=off`). CI runs it un-short via
`proxy:test`.
