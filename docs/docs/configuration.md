---
title: Configuration
description: CLI flags, environment variables, CodeMode options, limits, and transports.
---

# Configuration

The CLI uses Cobra and an instance-scoped Viper configuration. Flags take precedence over environment variables, which take precedence over defaults. `internal/templateinfo.Name` derives the `TEMPLATE_MCP_CODEMODE_*` environment prefix.

## Commands

| Command | Purpose |
| --- | --- |
| `template-mcp-codemode stdio` | Serve over STDIO for a local client-launched subprocess. |
| `template-mcp-codemode http` | Serve over Streamable HTTP. |
| `template-mcp-codemode --version` | Print version, commit, and build date. |

A local build prints `template-mcp-codemode dev (none) built unknown`. Release builds receive their metadata through linker flags.

## Global flags

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--log-level` | `TEMPLATE_MCP_CODEMODE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. |
| `--log-format` | `TEMPLATE_MCP_CODEMODE_LOG_FORMAT` | `text` | `text` or `json`. |

Invalid values fail at startup. Logs always go to stderr. STDIO reserves stdout for JSON-RPC.

## HTTP flags

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--addr` | `TEMPLATE_MCP_CODEMODE_ADDR` | `localhost:8080` | Listen address. |
| `--auth-token` | `TEMPLATE_MCP_CODEMODE_AUTH_TOKEN` | Empty | Demonstration shared bearer token; empty disables token validation. |
| `--insecure` | `TEMPLATE_MCP_CODEMODE_INSECURE` | `false` | Permit a non-loopback bind without authentication. |

A non-loopback bind without `--auth-token` fails unless `--insecure` explicitly permits unauthenticated exposure. Cross-origin protection is enabled independently of this bind check.

The shared token is not a production credential system. It does not validate a signed token, issuer, audience, expiry, or per-client scope. See [Security](security.md).

## Subject resolution by transport

| Mode | Resolver | Subject ID | Trust boundary |
| --- | --- | --- | --- |
| STDIO | `mcpserver.StaticSubject` | `local` | Ownership of the launched process. |
| HTTP with valid demo token | `mcpserver.ContextSubject` | `shared-token` | The demo SDK verifier sets `auth.TokenInfo.UserID` after constant-time token validation. The token value is not stored as identity. |
| HTTP on loopback without a token | `mcpserver.ContextSubject` | `development` | Explicit unauthenticated development identity passed through the MCP receiving bridge. |
| HTTP with `--insecure` and no token | `mcpserver.ContextSubject` | `development` | Explicit unauthenticated network identity passed through the MCP receiving bridge. |

For HTTP, the SDK authentication verifier supplies a stable, non-secret `auth.TokenInfo.UserID`. The `installHTTPSubject` receiving middleware reads `req.GetExtra().TokenInfo.UserID` from each MCP request and stores an `authz.Subject` with `authz.WithSubject` on the MCP handler context. `mcpserver.ContextSubject` resolves that value. Setting a value only on the outer `net/http` request context is not sufficient because the SDK establishes the receiving handler context. MCP tool input, Starlark source, request `_meta`, and unvalidated headers are not trusted identity sources.

## Template server options

`internal/mcpserver.New` has this template-owned API:

```text
New(options Options) (*mcp.Server, error)
```

`Options` contains:

| Field | Contract |
| --- | --- |
| `Version string` | Release version reported in the MCP implementation metadata. |
| `Deps Dependencies` | Shared host collaborators closed over by capability handlers. |
| `Logger *slog.Logger` | Server diagnostics; the CLI supplies a stderr logger. |
| `Resolver codemodemcp.InvocationResolver` | Required trusted-subject resolver selected by the transport. |
| `Runtime codemode.Options` | CodeMode authorizer, static capability filters, and execution/discovery limits. |

The CLI sets `Runtime.Authorizer` to `authz.AllowAll()` explicitly for the demo. `internal/mcpserver.New` does not replace a missing authorizer. Replace `AllowAll` when not every resolved subject may invoke every enabled capability.

The HTTP command calls `internal/mcpserver.New` once before serving and reuses the returned server for all sessions. Do not move construction into the per-session server factory. The shared runtime's `MaxConcurrentExecutions` limit then applies across the process, and shared dependencies are not recreated for each session.

## Runtime limits

Zero-valued `codemode.Limits` fields receive these bounded defaults during `Builder.Build`:

| Field | Default |
| --- | ---: |
| `MaxSourceBytes` | 65,536 bytes |
| `MaxExecutionSteps` | 1,000,000 |
| `MaxExecutionTime` | 5 seconds |
| `MaxNativeCalls` | 100 |
| `MaxValueDepth` | 32 |
| `MaxValueBytes` | 1,048,576 bytes |
| `MaxIntermediateValueBytes` | 8,388,608 bytes |
| `MaxSearchQueryBytes` | 256 bytes |
| `MaxSearchResults` | 20 |
| `MaxConcurrentExecutions` | 8 |

Override only the fields required by the deployment. A zero value does not mean unlimited:

```go
srv, err := mcpserver.New(mcpserver.Options{
	Version:  build.Version,
	Logger:   logger,
	Resolver: codemodemcp.StaticSubject(authz.Subject{ID: "local"}),
	Runtime: codemode.Options{
		Authorizer: authz.AllowAll(),
		Limits: codemode.Limits{
			MaxExecutionTime:        2 * time.Second,
			MaxNativeCalls:          25,
			MaxConcurrentExecutions: 4,
		},
	},
})
```

Limits are programmatic options. The template intentionally has no limit flags or `TEMPLATE_MCP_CODEMODE_*` limit variables. For exact accounting and validation rules, see the [CodeMode limits reference](https://meigma.github.io/codemode/reference/public-api/#limits).

## Upstream MCP adapter options

The template wrapper eventually calls the CodeMode adapter with the required three-argument signature:

```go
srv, err := codemodemcp.New(
	service,
	resolver,
	codemodemcp.Options{
		Implementation: &mcp.Implementation{
			Name:    templateinfo.Name,
			Title:   templateinfo.Title,
			Version: options.Version,
		},
		Logger: logger,
	},
)
```

The complete API is:

```text
mcpserver.New(service Service, resolver InvocationResolver, options Options) (*mcp.Server, error)
```

The third argument is required; use `mcpserver.Options{}` to accept upstream defaults. A nil `Options.Implementation` uses implementation name `codemode` and version `2`. `Options.Logger` is optional and a nil value uses the MCP SDK default. The template supplies its own implementation name, title, version, and logger.

See the [canonical `mcpserver` API reference](https://meigma.github.io/codemode/reference/public-api/#mcpserver) for service, resolver, and error contracts.

## Transports

Both subcommands use the same `internal/mcpserver` constructor:

- STDIO exchanges JSON-RPC over stdin/stdout and uses a process-owned static subject.
- Streamable HTTP uses SDK `TokenInfo.UserID`, a receiving-middleware bridge to per-request context subjects, cross-origin protection, a loopback default, and graceful shutdown.

To keep only one transport, delete the unused file in `internal/cli` and its registration in `internal/cli/root.go`. Do not move capability construction into transport code.
