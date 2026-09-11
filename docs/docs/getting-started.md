---
title: Getting started
description: Run the CodeMode server and compose the demo capability.
---

# Getting started

This tutorial starts the template as a local STDIO server, connects it to an MCP client, and composes two calls to `random.int` in one `execute` request.

## Install the repository toolchain

Clone a disposable checkout and provision the pinned Go 1.26.6 toolchain and project tools with [mise](https://mise.jdx.dev). The server module pins the official MCP Go SDK v1.7.0:

```sh
git clone https://github.com/meigma/template-mcp-codemode.git
cd template-mcp-codemode
mise install
```

Moon uses the mise-provided tools as system binaries. Python and uv for the documentation project are included; no separate install is required.

## Build the server

```sh
go build -o bin/template-mcp-codemode ./cmd/template-mcp-codemode
```

The final binary contains both the ordinary server and the CodeMode worker entry point. `codemode.ServeWorkerAndExit()` is the first statement of `main`, so CodeMode can re-execute this same binary for each program run.

## Connect over STDIO

Configure an MCP client that accepts the `mcpServers` shape. Replace the path below with the absolute path to your checkout:

```json
{
  "mcpServers": {
    "template-mcp-codemode": {
      "command": "/absolute/path/to/template-mcp-codemode/bin/template-mcp-codemode",
      "args": ["stdio"]
    }
  }
}
```

Restart or reload the client's MCP servers. STDIO uses the fixed non-secret subject ID `local`; ownership of the launched process is the authentication boundary. The process writes JSON-RPC only to stdout and sends diagnostics to stderr.

The client lists exactly three MCP tools:

- `search_api`
- `describe_api`
- `execute`

`random.int` is a CodeMode capability behind those tools. It is not a fourth MCP tool.

## Discover the capability

Ask the client to search for a capability that returns a random integer. The corresponding raw `search_api` input is:

```json
{"query":"random integer"}
```

The successful results include the exact dotted name and keyword-only signature:

```text
random.int(*, min: int, max: int)
```

Next, ask the client to describe that exact name. The raw `describe_api` input is:

```json
{"name":"random.int"}
```

The description reports required `min` and `max` integer inputs and an output dictionary with a `value` integer field. The Go input fields are `int64`, so values must fit the signed 64-bit range.

## Compose two calls

Ask the client to execute this program:

```python
def main():
    left = random.int(min=3, max=3)
    right = random.int(min=4, max=4)
    return {
        "left": left["value"],
        "right": right["value"],
        "total": left["value"] + right["value"],
    }
```

The raw `execute` input has one `source` string property:

```json
{
  "source": "def main():\n    left = random.int(min=3, max=3)\n    right = random.int(min=4, max=4)\n    return {\"left\": left[\"value\"], \"right\": right[\"value\"], \"total\": left[\"value\"] + right[\"value\"]}"
}
```

Each capability call returns a Starlark dictionary, so the program reads `left["value"]` and `right["value"]`. Equal lower and upper bounds make the result deterministic:

```json
{"result":{"left":3,"right":4,"total":7}}
```

CodeMode runs this source in a fresh worker process. Only the final converted value returned by the zero-argument `main()` function appears in the successful MCP result.

## Try Streamable HTTP

Start the HTTP transport on loopback:

```sh
go run ./cmd/template-mcp-codemode http --addr localhost:8080
```

The server logs its listening address to stderr and shuts down gracefully on `Ctrl-C`. Loopback without a token installs the explicit non-secret development subject ID `development` in trusted request context.

To exercise the demo bearer-token seam:

```sh
go run ./cmd/template-mcp-codemode http \
  --addr localhost:8080 \
  --auth-token development-only-token
```

After constant-time token validation, the demo SDK verifier sets the non-secret `auth.TokenInfo.UserID` to `shared-token`; it never uses the token value as identity. `installHTTPSubject` reads that ID from `req.GetExtra()` in receiving middleware, stores it with `authz.WithSubject` on the MCP handler context, and `mcpserver.ContextSubject` resolves it. This shared token is a demonstration, not production authentication.

Without a token in an allowed development mode, the same bridge installs `development`. The HTTP command builds one immutable CodeMode runtime and one MCP server before serving, then reuses that instance for every MCP session. An arbitrary value set only on the outer `net/http` request context is not the adapter's identity channel.

## Run project checks

```sh
moon run root:build
moon run root:test
moon run root:check
```

`root:check` covers formatting, linting, builds, tests, documentation, and the development proxy.

## Next steps

- [Add a capability](how-to/add-a-capability.md) and then remove `random.int`.
- Review the [configuration](configuration.md) reference.
- Read the [security](security.md) model before exposing HTTP or adding privileged handlers.
- Consult the [canonical CodeMode documentation](https://meigma.github.io/codemode/) for the full language and API contracts.
