---
title: Getting started
description: Clone the template and run the MCP server over both transports.
---

# Getting started

This tutorial takes you from a fresh clone to a running MCP server over both
transports. It assumes nothing beyond a terminal.

## Install prerequisites

The toolchain is provisioned by [mise](https://mise.jdx.dev) from `mise.toml` +
`mise.lock` and orchestrated by [Moon](https://moonrepo.dev/moon), which runs
every task against the mise-provided tools as `system` binaries on PATH. Install
mise, then provision the pinned tools from the repository root:

```sh
# Install mise: https://mise.jdx.dev/installing-mise.html
# Then, from the repository root, provision Go, Moon, golangci-lint, and more:
mise install
```

Building the documentation also needs Python and uv; mise provisions both from
the pinned versions, so no extra setup is required.

## Run over STDIO

The STDIO transport is what a local MCP client launches as a subprocess:

```sh
go run ./cmd/template-mcp stdio
```

The process speaks newline-delimited JSON-RPC over stdin/stdout and blocks until
the client closes the input stream or the process is signaled. That is expected:
it is a server, not a one-shot command. Diagnostics go to stderr; stdout carries
only protocol messages.

## Run over Streamable HTTP

The HTTP transport suits networked or containerized deployments. It binds
loopback by default:

```sh
go run ./cmd/template-mcp http --addr localhost:8080
```

You will see a `listening` log line on stderr. Press `Ctrl-C` for a graceful
shutdown.

## Call the demo tool

Both transports serve the same server, which registers one tool, `random_int`.
It takes `min` and `max` and returns a uniformly random integer in the inclusive
range `[min, max]`, or a tool-level error if `min > max`. Point your MCP client
at the server and call `random_int` to see structured output.

## Run the checks

Moon is the task front door:

```sh
moon run root:build
moon run root:test
moon run root:check   # format, lint, build, test, docs build, and the proxy checks
```

## Next steps

- [Add a tool](add-a-tool.md) of your own.
- Review the [configuration](configuration.md) reference.
- Read the [security](security.md) model before exposing the HTTP transport.
