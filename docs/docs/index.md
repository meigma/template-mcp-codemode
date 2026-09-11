---
title: template-mcp
slug: /
description: A Go template for building Model Context Protocol servers.
---

# template-mcp

`template-mcp` is a Go template for building [Model Context Protocol](https://modelcontextprotocol.io)
(MCP) servers on the official
[`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk).
It ships a transport-agnostic server with one demo tool (`random_int`) served
over either the STDIO or Streamable HTTP transport, plus Moon tasks, pinned CI,
dependency automation, secure-by-default settings, and an exercised release
pipeline.

## Documentation

This site follows the [Diátaxis](https://diataxis.fr/) structure:

- **[Getting started](getting-started.md)** — a tutorial: clone the template
  and run the server over both transports.
- **[Add a tool](add-a-tool.md)** — a how-to: replace `random_int` or add your
  own tool alongside it.
- **[Configuration](configuration.md)** — reference for the CLI flags,
  `TEMPLATE_MCP_*` environment variables, and transports.
- **[Security](security.md)** — an explanation of the template's
  secure-by-default choices and how to harden a real deployment.

The Go API reference is published on
[pkg.go.dev](https://pkg.go.dev/github.com/meigma/template-mcp).

## For generated projects

A project generated from this template should rewrite this page (and the pages
above) for the real server: its actual tools, the transport it kept, and its
operating and support notes. Update `docs/mkdocs.yml` (`site_url`, `repo_name`,
`repo_url`, `edit_uri`) to point at the generated repository.
