---
title: template-mcp-codemode
slug: /
description: A Go template for CodeMode-native Model Context Protocol servers.
---

# template-mcp-codemode

`template-mcp-codemode` is a Go template for building [Model Context Protocol](https://modelcontextprotocol.io) servers with [CodeMode](https://github.com/meigma/codemode). You register typed Go capabilities; an agent uses the fixed `search_api`, `describe_api`, and `execute` MCP tools to discover and compose them in bounded Starlark programs.

The template includes the `random.int` demo capability, STDIO and Streamable HTTP transports, explicit subject and authorization wiring, a hot-reload development proxy, Moon tasks, CI, documentation, and release configuration.

## Documentation

- **[Getting started](getting-started.md)** — clone the repository, run the server, and compose calls to `random.int`.
- **[Add a capability](how-to/add-a-capability.md)** — add a typed Go capability and remove the demo.
- **[Configuration](configuration.md)** — CLI flags, `TEMPLATE_MCP_CODEMODE_*` environment variables, runtime options, and default limits.
- **[Security](security.md)** — trusted identity, authorization, worker isolation, cancellation, and deployment boundaries.

Use the [canonical CodeMode documentation](https://meigma.github.io/codemode/) for the complete public Go API, fixed MCP tool contracts, supported Starlark surface, and runtime security model. The template-specific Go API is published at [pkg.go.dev](https://pkg.go.dev/github.com/meigma/template-mcp-codemode).

## Generated projects

After creating a project from this template, follow `DELETE_ME.md`. Rename the root and proxy modules, binary, client-visible implementation identity, environment prefix, repository and image references, and documentation metadata. Preserve the CodeMode dependency and worker entry points, replace the demo with real capabilities, and reset the changelog before the first release.
