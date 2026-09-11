# mcp-devproxy

`mcp-devproxy` keeps one client session open while rebuilding and replacing a STDIO MCP server. The client connects to the proxy once; the proxy watches source directories, builds a unique child binary, initializes it, swaps the active child, and forwards calls to the new process.

The proxy lives in the nested module `github.com/meigma/template-mcp-codemode/tools/proxy`, so its development dependencies do not enter the server module or release artifacts.

This template is CodeMode-native. Every healthy child exposes the same three outer MCP tools:

- `search_api`
- `describe_api`
- `execute`

Capabilities such as `random.int` live behind those tools. Adding or changing a capability normally leaves the three tool definitions unchanged.

## Quick start

The checked-in `.mcp.json` points Claude Code at a wrapper that builds the proxy with Moon and then replaces the wrapper process with the proxy:

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

Start Claude Code in the repository root and approve the project-scoped `dev` server. Edits under `cmd` or `internal` trigger rebuilds.

Two parts of the wrapper are required:

- `>&2` keeps build output away from stdout, which carries JSON-RPC.
- `proxy:build` declares its inputs and outputs, so Moon can skip a warm build without leaving a missing or stale proxy binary.

The proxy has defaults for this repository. A bare `mcp-devproxy` builds `./cmd/template-mcp-codemode` and runs the artifact with `stdio`. To provide every value explicitly:

```sh
mcp-devproxy \
  --build "go build -o {{artifact}} ./cmd/template-mcp-codemode" \
  --watch cmd --watch internal \
  --debounce 300ms \
  --quiesce 5s \
  --terminate 1s \
  -- {{artifact}} stdio
```

The child command after `--` runs after each successful build. It must contain `{{artifact}}`, because every build uses a new artifact path.

## Flags and environment

Flags take precedence over `MCP_DEVPROXY_*` environment variables, which take precedence over defaults.

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--build` | `MCP_DEVPROXY_BUILD` | `go build -o {{artifact}} ./cmd/template-mcp-codemode` | Build command template. It is split on whitespace without a shell and must contain `{{artifact}}`. |
| `--watch` | `MCP_DEVPROXY_WATCH` | `cmd`, `internal` | Recursively watched directory. Repeat the flag; the environment form is whitespace-separated. |
| `--dir` | `MCP_DEVPROXY_DIR` | Current directory | Working directory for the build command. |
| `--debounce` | `MCP_DEVPROXY_DEBOUNCE` | `300ms` | Time used to combine a burst of file events into one build. |
| `--quiesce` | `MCP_DEVPROXY_QUIESCE` | `5s` | Maximum wait for calls on the old child to finish before a swap. |
| `--terminate` | `MCP_DEVPROXY_TERMINATE` | `1s` | Wait for each shutdown step: close stdin, send `SIGTERM`, then send `SIGKILL`. |
| `--verbose` | `MCP_DEVPROXY_VERBOSE` | `false` | Enable debug logs, including build output, on stderr. |

The build command parser does not interpret shell quoting or arguments containing spaces. Use a wrapper executable when a build requires shell behavior.

The default child argv is `{{artifact}} stdio`. An override that omits `{{artifact}}` is rejected because it would continue to run a stale binary.

## Reload lifecycle

The lifecycle is `SERVING → BUILDING → STARTING → SWAPPING → SERVING`. A failure before the swap keeps the last healthy child active.

1. A debounced source change starts a build at a new artifact path. The running binary is never overwritten in place.
2. The proxy starts the candidate, performs the MCP handshake, lists its tools under a timeout, and validates every listed definition. The current child continues to serve during this health gate.
3. The proxy pauses new dispatches and buffers them within bounded count and time limits. It waits for in-flight calls up to `--quiesce`, switches routing to the candidate, and closes the previous child.
4. The proxy fingerprints and reconciles the outer MCP tool definitions. Removed definitions are unregistered; added or changed definitions are registered. A changed outer list can emit one coalesced `notifications/tools/list_changed`; an identical list emits nothing.
5. Buffered calls are sent to the new child only when the outer definition for that tool is unchanged. A changed or removed outer tool receives a stale-reload tool result instead.

Cold start serves an empty outer tool list while the first build runs. The first healthy child adds `search_api`, `describe_api`, and `execute`, which is an outer tool-list change and can notify the client.

## Capability changes and notifications

A CodeMode capability is catalog data behind the fixed outer tools. Adding `records.lookup`, renaming an input field, changing a summary, or removing `random.int` normally produces the same `tools/list` definitions for `search_api`, `describe_api`, and `execute`.

The proxy therefore does not promise `notifications/tools/list_changed` for a capability-only edit. This is expected, not a failed reload. After the swap:

1. Call `search_api` with task vocabulary that should find the changed capability.
2. Call `describe_api` with the exact returned dotted name and check its input and output fields.
3. Call `execute` with a zero-argument `main()` that uses the new shape.
4. Check the returned capability value, not only the absence of an error.

The existing client session already knows the three outer tools, so it can perform these calls without an outer tool-list refresh.

The stale-call gate also compares outer tool definitions. It cannot detect that the catalog or handler semantics behind an unchanged `execute` definition changed. A call buffered during a capability-only swap may run on the new child. Do not use the proxy as a transactional deployment boundary, and do not assume its outer-definition fingerprint protects a non-idempotent capability across a catalog edit.

## Failure behavior

| Failure | Behavior |
| --- | --- |
| Build fails | Keep the current child, log the compiler output, and remain in `SERVING`. |
| Candidate fails initialization or outer tool validation | Terminate the candidate and keep the current child. |
| First build or child fails | Keep the downstream session with an empty outer tool set and retry with backoff. |
| Active child crashes | Restart the last good artifact without rebuilding, using exponential backoff. |
| Call arrives during a swap | Buffer it within the configured count and timeout. |
| Buffered outer tool changed or disappeared | Return a readable stale-reload tool result instead of forwarding it. |
| Old child call outlives the quiesce period | Return an interruption result that says it may have executed; never replay it automatically. |
| Client exits or proxy receives a signal | Cancel the current cycle, close children, and exit. |

Reloads become visible when a build and swap complete. They are not synchronized with an agent's conversational turn.

## Forwarding limits

The proxy forwards tools only. A child that uses other MCP features sees these differences:

- Sampling and elicitation return errors because the upstream client has no handlers for them.
- `roots/list` returns method not found.
- Prompts and resources appear empty downstream. The proxy logs a warning when the child advertises either.
- Progress tokens are removed from forwarded calls, but request cancellation still propagates.
- Child initialization instructions are not forwarded because the downstream session exists before the first child.

Child MCP logging is forwarded. The client's latest `logging/setLevel` value is replayed to each replacement child.

## Observability

All proxy logs go to stderr. Stdout is the downstream JSON-RPC stream. A child's stderr is copied to proxy stderr, and the child inherits the proxy environment.

Use `--verbose` to include build output and lifecycle details.

For a temporary reload log while using Claude Code, append a stderr redirect to the wrapper command:

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

Remove the redirect after the investigation.

## Manual CodeMode reload checks

Run these checks after changing the proxy, its child handshake, or the CodeMode adapter, and when upgrading a client's major version.

### Added capability

1. In the live session, call `search_api` for `random integer`, describe `random.int`, and execute it once.
2. Add a capability with a unique search term and a handler that returns an unguessable value.
3. Wait until stderr shows a successful build and swap.
4. Call `search_api` with the unique term, then `describe_api` with the returned exact name.
5. Call `execute` and return the capability's value from `main()`.

Pass when search and description show the new capability and `execute` returns the unguessable value without reconnecting. Do not require `tools/list_changed`; the outer definitions are unchanged.

### Input-shape change

1. Change an existing capability's input field name or required shape without renaming the capability.
2. Wait for a successful swap.
3. Repeat `search_api` and `describe_api`; confirm the new signature and field shape.
4. Run an `execute` program with the new keyword arguments and check its final result.
5. Optionally run the old source and confirm CodeMode rejects its arguments rather than dispatching the handler.

Pass when description and execution use the new shape. An outer list-change notification is not part of this check.

### Removed capability

1. Remove a capability registration and wait for a successful swap.
2. Search for its exact name and task vocabulary.
3. Describe the old exact name.
4. Execute a program that calls the old name.

Pass when search no longer returns it, description reports `capability not found`, and execution does not dispatch the removed handler.

### Cold start

Start a new client session. The initial outer list can be empty while the first build runs. Pass when the first healthy child makes `search_api`, `describe_api`, and `execute` available and they can discover and execute `random.int` without reconnecting.

## Develop the proxy

```sh
moon run proxy:check
go test -short ./...
```

The short command skips the slow end-to-end test. The end-to-end test performs a real build and starts real child processes with network module lookup disabled; CI runs it through the proxy test task.
