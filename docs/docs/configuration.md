---
title: Configuration
description: CLI flags, environment variables, and transports.
---

# Configuration

The CLI is built with Cobra and Viper. Every flag is also settable through an
environment variable named after the application: the `TEMPLATE_MCP_*` prefix is
derived from the binary name, so renaming the app renames the variables. Flags
take precedence over environment variables, which take precedence over defaults.

## Commands

| Command | Purpose |
|---------|---------|
| `template-mcp stdio` | Serve over the STDIO transport (local subprocess). |
| `template-mcp http` | Serve over the Streamable HTTP transport (networked). |
| `template-mcp --version` | Print version, commit, and build date. |

A local build prints `template-mcp dev (none) built unknown`; GoReleaser injects
the real values at release time.

## Global flags

These apply to every command.

| Flag | Environment | Default | Meaning |
|------|-------------|---------|---------|
| `--log-level` | `TEMPLATE_MCP_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, or `error`. |
| `--log-format` | `TEMPLATE_MCP_LOG_FORMAT` | `text` | Log format: `text` or `json`. |

Logs always go to stderr. On the STDIO transport, stdout is reserved for the
JSON-RPC message stream, so nothing else may write to it. An unrecognized level
or format fails fast at startup.

## `http` flags

| Flag | Environment | Default | Meaning |
|------|-------------|---------|---------|
| `--addr` | `TEMPLATE_MCP_ADDR` | `localhost:8080` | Address to listen on. |
| `--auth-token` | `TEMPLATE_MCP_AUTH_TOKEN` | _(empty)_ | DEMO-ONLY shared bearer token; empty disables auth. |
| `--insecure` | `TEMPLATE_MCP_INSECURE` | `false` | Allow binding a non-loopback address without authentication (UNSAFE). |

Binding a non-loopback address (for example `0.0.0.0`) without authentication is
refused at startup unless you set `--auth-token` or pass `--insecure`. See
[Security](security.md) for the full rationale and the production upgrade path.

## Transports

Both subcommands build the same `internal/mcpserver` server and differ only in
how they connect it to a transport:

- **STDIO** (`internal/cli/stdio.go`) — the client launches the process and
  speaks JSON-RPC over stdin/stdout. Authorization is out of scope; the process
  inherits any credentials from its environment.
- **Streamable HTTP** (`internal/cli/http.go`) — for remote or containerized
  clients. Cross-origin protection is enabled and the bind defaults to loopback.

To keep only one transport, delete the unused subcommand file and its single
registration line in `internal/cli/root.go`.
