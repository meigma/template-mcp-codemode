---
title: Security
description: Identity, authorization, execution, cancellation, and deployment boundaries.
---

# Security

CodeMode restricts the program language and executes each program in a fresh worker process, but the host still controls identity, authorization, capability behavior, and operating-system isolation.

## Keep STDIO stdout protocol-only

STDIO uses stdout for JSON-RPC. A `fmt.Println` call or logger pointed at stdout corrupts the protocol stream. The template sends logs and diagnostics to stderr, including JSON-formatted logs. Keep every code path used by STDIO free of non-protocol stdout writes.

CodeMode workers also use standard input and output for their private protocol. `codemode.ServeWorkerAndExit()` writes no diagnostics and must remain the first statement of `main` and applicable `TestMain` functions.

## Establish identity outside model-controlled data

The adapter resolves a trusted `authz.Subject` before search, description, or execution:

- STDIO uses `StaticSubject` with subject ID `local`. This is appropriate only when ownership of the local process is the authentication boundary.
- HTTP uses `ContextSubject`. The SDK authentication verifier places a stable, non-secret identity in `auth.TokenInfo.UserID`.
- The `installHTTPSubject` receiving middleware reads `req.GetExtra().TokenInfo.UserID` from each MCP request, stores an `authz.Subject` with `authz.WithSubject` on the MCP handler context, and then lets `ContextSubject` resolve it.
- A valid demo bearer token produces the fixed user ID `shared-token`. The token value is never used as the user or subject ID.
- Allowed loopback and explicit `--insecure` requests without a token pass the fixed development ID `development` through the same bridge.

The MCP SDK establishes the receiving handler context, so setting an arbitrary value only on the outer `net/http` request context is not sufficient. Program source, capability arguments, MCP `_meta`, and unvalidated request headers cannot establish or replace identity.

## Replace demo authentication before deployment

HTTP defaults to `localhost:8080` and enables standard-library cross-origin protection. A non-loopback address without a token is refused unless `--insecure` explicitly permits unauthenticated exposure.

Cross-origin protection mitigates browser-origin attacks; it does not authenticate direct clients. The shared-token seam only performs constant-time comparison with one configured secret. It does not validate a signature, issuer, audience, expiry, revocation state, or client-specific scope.

For production, implement the MCP authorization requirements for an OAuth 2.1 protected resource, including protected-resource metadata, audience-restricted access tokens, PKCE with S256 where applicable, and validation against a trusted authorization server. The real verifier must set `auth.TokenInfo.UserID` to the authenticated caller's stable, non-secret identity. Keep the receiving bridge and `ContextSubject`; do not replace them with an outer HTTP context wrapper.

STDIO servers do not use HTTP OAuth. They obtain any credentials needed by handlers from the launched process's environment or another local trust channel.

## Treat `AllowAll` as an explicit demo policy

The CLI passes `authz.AllowAll()` through `mcpserver.Options.Runtime.Authorizer`. CodeMode has no default authorizer, and the template constructor does not silently create one.

`AllowAll` permits every validated native call for every resolved subject. It is not authentication. Replace it when authorization depends on subject, stable capability ID, capability name, or canonical arguments. Keep authorization failures coarse at the client boundary and record trusted diagnostic detail only in protected host logs.

## Discovery is not authorization-filtered

`search_api` and `describe_api` require a resolved subject, but they do not run per-capability authorization policy. Every authenticated subject can discover every capability that is statically enabled in that runtime.

Do not put credentials, policy facts, tenant identifiers, or sensitive examples in capability names, summaries, descriptions, search terms, or field names. If a deployment must hide a capability's existence, disable it through `codemode.Options.DisabledCapabilities` when building that deployment. Per-invocation authorization still applies to every native call made by `execute`.

## Preserve worker entry-point ordering

CodeMode re-executes the host binary for its build probe and each `execute` worker. Keep this as the first statement of `main`:

```go
codemode.ServeWorkerAndExit()
```

Place flag parsing, credentials, database connections, service clients, authorizers, handlers, and transport construction after it. Test packages that call `Builder.Build` need the same first statement in `TestMain`.

Go package initialization runs before `main`. Do not initialize credentials, open privileged resources, or perform irreversible work in package initializers. In worker mode, `ServeWorkerAndExit` can call `os.Exit`, so deferred functions do not run.

## Understand the worker boundary

Each `execute` call creates a fresh Starlark interpreter in a re-executed child process. Module loading is disabled. The language surface contains standard Starlark built-ins, `sum`, `json`, `math`, and the statically enabled capability namespaces. Native calls are rejected during top-level loading and allowed only from a zero-argument `main()`.

Only `main()`'s final converted value is returned. Printed text, globals, and interpreter-local intermediate values are discarded. Interpreter state does not cross `execute` calls.

The worker runs as the same operating-system user as the host binary. Its restricted environment and lack of file, network, environment, or process built-ins reduce reachability, but they are not tenant isolation. CodeMode does not provide an operating-system CPU, heap, filesystem, credential, or network boundary. Add containers, workload isolation, and operating-system resource controls when those boundaries are required.

## Treat handlers as privileged host code

Capability handlers and authorizers run in the parent process with the host's privileges, not inside the Starlark worker. CodeMode binds and canonicalizes arguments before authorization and dispatches the handler only after authorization succeeds.

When a request is canceled or exceeds `MaxExecutionTime`, CodeMode can close, kill, and reap the worker. Cancellation of parent Go code remains cooperative. CodeMode cannot forcibly stop a handler or authorizer goroutine and cannot undo side effects that already occurred. A non-cooperative handler can continue consuming host resources after `execute` returns.

Handlers and authorizers must:

- honor the supplied context for I/O, locks, waits, and downstream calls;
- return promptly after cancellation;
- bound their own retries, memory, network, and storage use;
- be safe for concurrent calls against the shared immutable server;
- make non-idempotent effects explicit and independently safe; and
- avoid returning credentials or trusted diagnostic details to the caller.

## Configure bounded execution programmatically

CodeMode supplies bounded defaults for source size, bytecode steps, elapsed time, native call count, value depth and size, cumulative intermediate values, search input and results, and concurrent executions. Configure overrides through `mcpserver.Options.Runtime.Limits`; there are no CLI limit flags or environment variables.

The HTTP transport constructs one runtime and MCP server before serving and shares them across sessions. `MaxConcurrentExecutions` therefore bounds worker spawn attempts and live workers across the process rather than resetting for each session. Each `execute` call receives fresh per-execution budgets.

These limits do not bound handler-owned resources or impose operating-system CPU and memory quotas. See the [canonical CodeMode security model](https://meigma.github.io/codemode/explanation/security-model/) for exact execution, error-projection, and value-crossing behavior.

## Supply chain and container

- The image is assembled from a signed melange-built Wolfi package and runs as non-root uid/gid 65532 with no shell.
- CI uses minimal token permissions, digest-pinned actions, and disabled checkout credential persistence.
- The release configuration produces checksums and SBOMs, isolates provenance signing in a reusable workflow, and configures a keyless Cosign image signature.
- The scheduled image scan uploads SARIF to GitHub code scanning.
- Dependabot covers GitHub Actions, both Go modules, and the docs project.
- Repository settings configure signed commits, squash-only merges, protected tags, immutable releases, and private vulnerability reporting.

These are configured paths, not evidence that this repository has already published a release. The release baseline is `0.0.0`, with `0.1.0` pending as the first release.

Report vulnerabilities through the private process in the repository [security policy](https://github.com/meigma/template-mcp-codemode/blob/master/SECURITY.md).
