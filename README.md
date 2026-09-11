# template-mcp

`template-mcp` is a Go template for building [Model Context Protocol](https://modelcontextprotocol.io) (MCP) servers.
It is built on the official [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) and ships with the protocol and security best practices that an MCP server should have on day one.

The template exposes a single demo tool, `random_int`, and demonstrates serving it over two transports from the same server code:

- **Local** — the STDIO transport (`template-mcp stdio`), which clients spawn as a subprocess.
- **Networked** — the Streamable HTTP transport (`template-mcp http`), suitable for remote and containerized deployments.

Generated projects keep the transport they need and delete the other (see [Choosing a transport](#choosing-a-transport)).

## Local Bootstrap

Prerequisites:

- [mise](https://mise.jdx.dev) — provisions every pinned tool from `mise.toml` +
  `mise.lock`: Go, Moon, Python + uv (for the MkDocs docs project), the
  `golangci-lint` and `mockery` CLIs, and `melange`/`apko`/`cosign` for releases.
  Run `mise install` once; there is nothing else to install by hand.
- Docker — only to build and scan the container image locally; not needed to run
  the server itself.

Tool versions live in `mise.toml`; `mise.lock` records a per-platform download URL
and checksum for each (and, for the aqua-backed CLIs, cosign/SLSA/GitHub-attestation
verification). `mise install` runs with `locked = true`, so it **fails closed** if a
tool lacks a pre-resolved, checksummed entry for the current platform — replacing the
former Proto `checksum-url` pins. Moon runs every task against these tools as `system`
binaries on PATH and manages no toolchain itself. To bump a tool, edit its version in
`mise.toml`, run `mise lock --platform linux-x64,linux-arm64,macos-x64,macos-arm64`,
and commit `mise.toml` + `mise.lock`.

```sh
# Install mise (https://mise.jdx.dev/installing-mise.html), then from the repo
# root provision every pinned tool (Go, Moon, the dev CLIs, and the release stack):
mise install
```

After creating a new repository from this template, replace the placeholder names before doing feature work:

```sh
go mod edit -module github.com/meigma/YOUR_REPO
mv cmd/template-mcp cmd/YOUR_BINARY
```

Then update `template-mcp` references in the Moon tasks, GoReleaser config, `ghd.toml`, README, and package docs.
The full first-setup checklist lives in [DELETE_ME.md](DELETE_ME.md).

## Running the Server

Run the server over STDIO (the mode a local MCP client launches):

```sh
go run ./cmd/template-mcp stdio
```

Run the server over Streamable HTTP, bound to loopback by default:

```sh
go run ./cmd/template-mcp http --addr localhost:8080
```

Both subcommands build the same `internal/mcpserver` server and differ only in how they connect it to a transport.

## Hot Reload During Development

The repository ships a dev proxy (`tools/proxy`) and a checked-in `.mcp.json` that wires it up, so developing the server with Claude Code needs no setup.
Start `claude` in the repository root, approve the project-scoped `dev` server, and edit the server source: the proxy rebuilds on save and swaps the running server behind the live session — new and changed tools appear on the next conversation turn with no reconnect.
See [tools/proxy/README.md](tools/proxy/README.md) for how it works and its flags.

## The Demo Tool

The template registers one tool, `random_int`, in `internal/mcpserver`.
It takes `min` and `max` arguments and returns a uniformly random integer in the inclusive range `[min, max]`.

The tool is deliberately small but exercises the parts of the protocol you are most likely to use:

- Typed input and output structs, from which the SDK derives the JSON Schemas automatically.
- Structured output, marshaled from the typed return value by the SDK.
- The tool-error convention: an invalid range (`min > max`) returns a tool-level error result (`IsError`) rather than a JSON-RPC protocol error.

Replace `random_int` with your own tool, or add more tools alongside it. The server and transport code do not change when you do.

A tool that needs shared collaborators (a database handle, an HTTP client, a config struct) gets them through the `Dependencies` struct on `mcpserver.Options`: add fields there, and each `registerXxx` function receives them via `Options.Deps`. Because dependencies flow through `Options`, the server stays transport-agnostic. See the [Add a tool](https://meigma.github.io/template-mcp/add-a-tool/) guide for a worked example.

## Choosing a Transport

The server in `internal/mcpserver` knows nothing about transports. Each transport is a Cobra subcommand in its own file:

- `internal/cli/stdio.go` — the `stdio` subcommand.
- `internal/cli/http.go` — the `http` subcommand.

To keep only one transport, delete the unused file and remove its single registration line in `internal/cli/root.go`. The tool and server code are untouched.

## Security & Best Practices

The template bakes in the practices that an MCP server must have. Preserve them as you build on it.

- **stdout is reserved for JSON-RPC.** Over the STDIO transport, stdout carries protocol messages only. Writing anything else to stdout — a stray `fmt.Println`, a logger pointed at `os.Stdout` — silently corrupts the stream and is the most common way a stdio server breaks. The template logs to `os.Stderr` only; keep all logging and diagnostics on stderr.
- **Origin verification and a loopback default for HTTP.** The `http` transport wraps the SDK handler in the standard library's cross-origin protection to defend against DNS-rebinding and CSRF from browsers, and `--addr` defaults to `localhost:8080`. Binding to a non-loopback address exposes the server to the network and is an explicit, security-relevant decision.
- **The HTTP transport fails closed off loopback.** Cross-origin protection stops malicious browsers, not direct clients such as `curl`. So binding a non-loopback address (for example `0.0.0.0`) with no authentication is refused at startup unless you either set `--auth-token` or pass `--insecure` to opt into an unauthenticated, network-exposed server. The container image defaults to `--insecure` so the demo runs out of the box; remove it and supply real authentication before deploying.
- **The bearer-auth seam is demo-only.** The HTTP transport includes a minimal, flag-gated bearer-token check that is off by default and exists to show where authorization belongs. It is not production authorization. A production server needs a real OAuth 2.1 resource server: protected-resource metadata (RFC 9728), audience-restricted tokens (RFC 8707), and PKCE with S256. Validate token signature, expiry, and audience against a trusted authorization server.
- **Authorization is HTTP-only.** Per the MCP specification, authorization applies to HTTP transports only. STDIO servers must not use OAuth; they take any credentials they need from the environment of the process that launched them.

## Common Tasks

Moon is the standard task front door:

```sh
moon run root:format       # check formatting (golangci-lint fmt --diff)
moon run root:format-fix   # apply formatting
moon run root:lint
moon run root:build
moon run root:test
moon run root:check        # format, lint, build, test, docs build, and proxy checks
```

CI runs the same aggregate check:

```sh
moon ci --summary minimal
```

Preview the documentation site locally with live reload:

```sh
moon run docs:serve        # serves on http://127.0.0.1:8000
```

The CLI entrypoint uses Cobra and Viper in the same shape as other Meigma CLIs: `cmd/template-mcp` stays thin, `internal/cli` owns command construction, and Viper-backed flags such as the HTTP address can also be supplied through `TEMPLATE_MCP_*` environment variables.

```sh
go run ./cmd/template-mcp --version
go run ./cmd/template-mcp stdio
go run ./cmd/template-mcp http --addr localhost:8080
go test ./...
```

A local build reports `template-mcp dev (none) built unknown` — GoReleaser
injects the real version, commit, and date at release time.

## Logging and Observability

Both transports log to stderr (never stdout, which the stdio transport reserves
for JSON-RPC). Two persistent flags control logging, and each is also settable
through an environment variable:

```sh
go run ./cmd/template-mcp http --log-level debug --log-format json
TEMPLATE_MCP_LOG_LEVEL=debug go run ./cmd/template-mcp stdio
```

- `--log-level` (`TEMPLATE_MCP_LOG_LEVEL`): `debug`, `info` (default), `warn`, or `error`.
- `--log-format` (`TEMPLATE_MCP_LOG_FORMAT`): `text` (default) or `json`.

The http transport logs a `listening` line on startup and a clean-shutdown pair
on exit. Metrics and tracing are intentionally out of scope for the template;
the `Options.Logger` seam in `internal/mcpserver` is where richer instrumentation
would attach.

## Container Image

The image is built with [melange](https://github.com/chainguard-dev/melange) — which
compiles the binary into a signed Wolfi apk — and [apko](https://github.com/chainguard-dev/apko),
which assembles that apk plus a minimal Wolfi base into a multi-arch, nonroot OCI
image (uid/gid 65532, ca-certificates, tzdata, no shell). It mirrors the former
`distroless/static-debian12:nonroot` posture. Build it locally and load it into Docker:

```sh
mise run image-local                       # builds template-mcp:dev for the host arch
docker run --rm template-mcp:dev --version
```

The Wolfi base intentionally floats to the latest (fresh CA bundle/timezones, low CVE
surface); the exact resolved package versions are recorded in the per-build SBOM and
provenance attestation rather than pinned in a lockfile. version/commit/date are
stamped into the binary via melange's `--vars-file`, mirroring GoReleaser.

Containers are the networked deployment, so the image defaults to
`http --addr 0.0.0.0:8080 --insecure`, which runs the demo unauthenticated;
`--insecure` is required because the server otherwise refuses to bind a non-loopback
address without authentication. Before deploying, drop `--insecure` and supply real
authorization (see the security expectations above). Override the default at runtime,
for example `docker run --rm template-mcp:dev stdio`.

## CI and Security

The default CI workflow keeps permissions minimal, pins external actions, disables checkout credential persistence, and delegates checks to Moon.
It uses GitHub-hosted dependency caches for Go, golangci-lint, and uv download artifacts while leaving Moon remote caching as an optional follow-up for repositories that need a shared task-output cache.
The docs workflow builds the MkDocs site on pull requests and deploys `docs/build` to GitHub Pages from the default branch.
The scheduled security scan workflow builds the local melange/apko image weekly, scans it for high/critical fixed vulnerabilities, and uploads SARIF results to GitHub code scanning.
Dependabot covers GitHub Actions, the root and dev-proxy Go modules, and the docs uv project.

Repository settings live in `.github/repository-settings.toml`.
They default to immutable releases, private vulnerability reporting, signed commits, squash-only merges, GitHub Pages workflow publishing, and protected tags.

## Release Layer

Release automation is enabled for the template application so this repository proves the full binary and container release lifecycle before generated projects inherit it.
Repositories generated from the template should update the release app credentials, package names, asset patterns, container image name, and `ghd.toml` signer workflow before cutting their first release.

The release path is:

- Release Please opens and maintains the release PR.
- Release Please creates a draft GitHub release and tag after merge.
- Release Dry Run rehearses the GoReleaser binary path and the native-runner melange/apko container build path on pull requests.
- GoReleaser builds binaries, checksums, and SBOMs without publishing directly.
- The release workflow uploads assets to the draft release; binary checksum provenance is attested from an isolated reusable workflow (`.github/workflows/attest.yml`).
- The release workflow builds per-arch signed apks with melange on native GitHub-hosted runners, assembles and publishes `ghcr.io/meigma/template-mcp:vX.Y.Z` as a multi-platform apko manifest, signs it with keyless cosign, attaches a GitHub-native SBOM attestation, and attests image provenance from the same isolated `attest.yml` workflow.
- Provenance is generated in `attest.yml` so its signing key is unreachable by the build jobs (SLSA Build L3). Verify with `gh attestation verify <artifact-or-oci-ref> --signer-workflow <repo>/.github/workflows/attest.yml`; the keyless cosign image signature is still issued by `release.yml`.
- A human inspects the draft release before publication.

The root `ghd.toml` matches the default GoReleaser output so generated projects can be installed with `ghd` once the release workflow runs.
After cloning this template, update `provenance.signer_workflow`, package names, asset patterns, binary paths, and image names to match the new repository and binary name.

## Documentation

Full documentation is published at <https://meigma.github.io/template-mcp/>: a getting-started tutorial, an add-a-tool how-to, a configuration reference, and the security model. The Go API reference is on [pkg.go.dev](https://pkg.go.dev/github.com/meigma/template-mcp). Preview the site locally with `moon run docs:serve`.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution guidelines, local setup expectations, and pull request workflow.

## Security

See [SECURITY.md](SECURITY.md) for supported versions and the private vulnerability reporting path.

## License

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE))
- MIT license ([LICENSE-MIT](LICENSE-MIT))

at your option (`SPDX-License-Identifier: Apache-2.0 OR MIT`).

Unless you explicitly state otherwise, any contribution intentionally submitted for inclusion in this project by you, as defined in the Apache-2.0 license, shall be dual licensed as above, without any additional terms or conditions.
