---
title: Security
description: The template's secure-by-default choices and how to harden a deployment.
---

# Security

The template bakes in the practices an MCP server should have on day one.
This page explains the reasoning so you can preserve the guarantees as you build.

## stdout is reserved for JSON-RPC

Over the STDIO transport, stdout carries protocol messages only. A stray
`fmt.Println` or a logger pointed at `os.Stdout` silently corrupts the stream —
the most common way a stdio server breaks. The template logs to stderr only;
keep all logging and diagnostics there. The `--log-format=json` option still
writes to stderr.

## HTTP defaults to loopback with cross-origin protection

The `http` transport wraps the SDK handler in the standard library's
cross-origin protection to defend against DNS-rebinding and CSRF from browsers,
and `--addr` defaults to `localhost:8080`. Binding a non-loopback address
exposes the server to the network and is a deliberate, security-relevant choice.

## The HTTP transport fails closed off loopback

Cross-origin protection stops malicious browsers, not direct clients such as
`curl`. So binding a non-loopback address (for example `0.0.0.0`) with no
authentication is refused at startup unless you either set `--auth-token` or pass
`--insecure` to opt into an unauthenticated, network-exposed server. The
container image defaults to `--insecure` so the demo runs out of the box; remove
it and supply real authentication before deploying.

## The bearer-auth seam is demo-only

The HTTP transport includes a minimal, flag-gated bearer-token check that is off
by default and exists to show where authorization belongs. It compares a single
shared secret in constant time. It is **not** production authorization.

A production server needs a real OAuth 2.1 resource server:

- protected-resource metadata (RFC 9728),
- audience-restricted tokens (RFC 8707),
- PKCE with S256,
- and validation of token signature, expiry, and audience against a trusted
  authorization server.

Per the MCP specification, authorization applies to HTTP transports only. STDIO
servers must not use OAuth; they take any credentials they need from the
environment of the process that launched them.

## Supply chain and container

- The container builds a static binary into a non-root, digest-pinned
  [distroless](https://github.com/GoogleContainerTools/distroless) runtime
  image.
- CI keeps token permissions minimal, pins every action by digest, and disables
  checkout credential persistence.
- Releases publish checksums and SBOMs and attach GitHub-native attestations to
  both the binary checksums and the container manifest.
- A weekly scheduled scan checks the image for vulnerabilities, secrets, and
  misconfigurations and uploads results to GitHub code scanning.
- Dependabot updates GitHub Actions, both Go modules, and the docs project.
- Repository settings default to signed commits, squash-only merges, protected
  tags, immutable releases, and private vulnerability reporting.

See [SECURITY.md](https://github.com/meigma/template-mcp/blob/master/SECURITY.md)
for the vulnerability reporting policy.
