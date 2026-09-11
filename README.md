# template-mcp-codemode

`template-mcp-codemode` is a Go template for building [Model Context Protocol](https://modelcontextprotocol.io) servers with [CodeMode](https://github.com/meigma/codemode). Instead of registering one MCP tool per operation, you register typed Go capabilities. An agent discovers them and composes several calls in one bounded Starlark program.

Every server created from this template exposes exactly three MCP tools:

- `search_api` finds capabilities by name, summary, and search terms.
- `describe_api` returns the exact input and output shape for one capability.
- `execute` runs a Starlark program and returns the value from its zero-argument `main()` function.

The included `random.int` capability demonstrates typed input and structured output over both STDIO and Streamable HTTP.

## Local bootstrap

Prerequisites:

- [mise](https://mise.jdx.dev), which provisions the pinned Go 1.26.6 toolchain, Moon, Python and uv, the development CLIs, and the release tools from `mise.toml` and `mise.lock`. The server module pins the official MCP Go SDK v1.7.0.
- Docker, only for local container builds and scans.

From the repository root:

```sh
mise install
```

`mise install` runs with locked tool resolution. To update a tool, edit `mise.toml`, regenerate `mise.lock` for the supported platforms, and commit both files.

## Run the server

Run the local STDIO transport:

```sh
go run ./cmd/template-mcp-codemode stdio
```

Run Streamable HTTP on its loopback default:

```sh
go run ./cmd/template-mcp-codemode http --addr localhost:8080
```

Both commands build one immutable CodeMode runtime through `internal/mcpserver`. The HTTP command constructs the runtime and MCP server once at startup and shares them across sessions; it does not rebuild the capability catalog per request or per session.

For a local MCP client, build the binary and configure its absolute path:

```sh
go build -o bin/template-mcp-codemode ./cmd/template-mcp-codemode
```

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

## Compose capability calls

Ask the client to find and describe a random integer capability, then run this program through `execute`:

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

Each `random.int(...)` call returns a dictionary whose `value` entry is a signed 64-bit integer. The fixed bounds make this example deterministic. The successful structured result is:

```json
{"result":{"left":3,"right":4,"total":7}}
```

Only `main()`'s final converted value is returned. Intermediate capability results remain inside the worker and do not enter the model's context.

## Add capabilities

Capabilities live in `internal/mcpserver`. A capability combines:

- a stable deployment and authorization ID;
- a dotted Starlark name such as `random.int`;
- discovery metadata;
- non-pointer input and output structs; and
- a typed Go handler that receives `context.Context`, the trusted `authz.Subject`, and the input value.

Use direct exported fields and supported scalar input types. In particular, integer inputs use `int64`, not platform-sized `int`. CodeMode accepts JSON struct tags for capability fields and rejects unrelated struct tags. See [Add a capability](docs/docs/how-to/add-a-capability.md) for the repository procedure and the [canonical CodeMode public API reference](https://meigma.github.io/codemode/reference/public-api/) for the complete type contract.

## Worker entry points

`codemode.ServeWorkerAndExit()` must be the first statement of the final binary's `main` function. Keep it before flag parsing, credential loading, client construction, and all other host setup. CodeMode re-executes the binary for its worker process; late wiring can run privileged host setup in the worker or make the build-time worker probe fail.

Every test binary that calls `Builder.Build` also needs this first statement:

```go
func TestMain(m *testing.M) {
	codemode.ServeWorkerAndExit()
	os.Exit(m.Run())
}
```

Add one applicable `TestMain` per Go package. Do not put setup before the worker call.

## Identity and authorization

The template keeps authentication identity outside program source, tool arguments, and MCP metadata:

- STDIO uses `mcpserver.StaticSubject` with the non-secret subject ID `local`. Process ownership is the authentication boundary.
- HTTP uses `mcpserver.ContextSubject`. The receiving MCP middleware reads the SDK-authenticated `req.GetExtra().TokenInfo.UserID`, stores that non-secret identity with `authz.WithSubject`, and then lets the CodeMode adapter resolve it. Setting an arbitrary value only on the outer `net/http` request context is not sufficient.
- The demo verifier sets `TokenInfo.UserID` to `shared-token`; the token value is not used as identity. Allowed loopback and explicitly insecure unauthenticated modes send `development` through the same receiving bridge.

The CLI passes `authz.AllowAll()` explicitly in `codemode.Options` so the demo is runnable. `AllowAll` is authorization, not authentication, and it permits every capability call for every resolved subject. `internal/mcpserver.New` does not supply a hidden fallback authorizer. Replace this demo policy and the HTTP authentication seam before a real deployment.

Discovery is not filtered by per-invocation authorization. An authenticated subject can search and describe every statically enabled capability; authorization runs for each native capability call during `execute`. Do not put secrets or tenant-sensitive data in capability names, summaries, descriptions, search terms, or field names. Disable a capability at build time if its existence must be hidden.

## Choose a transport

Transport code is isolated in `internal/cli`:

- `stdio.go` serves clients that spawn the binary as a local subprocess.
- `http.go` serves networked and containerized clients.

To keep one transport, delete the unused file and its registration in `internal/cli/root.go`. The capability registrations in `internal/mcpserver` do not change.

## Hot reload during development

The checked-in `.mcp.json` starts the development proxy in `tools/proxy`. Start Claude Code in the repository root, approve the project-scoped `dev` server, and edit `cmd` or `internal`; the proxy rebuilds and swaps the child process behind the existing session.

CodeMode capability changes do not change the outer definitions of `search_api`, `describe_api`, or `execute`, so they normally do not emit `notifications/tools/list_changed`. Validate a reload by calling `search_api`, then `describe_api`, then `execute` and checking the capability result. See [the proxy guide](tools/proxy/README.md) for the exact workflow and its fidelity limits.

## Configuration and logging

Cobra flags take precedence over `TEMPLATE_MCP_CODEMODE_*` environment variables, which take precedence over defaults. Common commands include:

```sh
go run ./cmd/template-mcp-codemode --version
go run ./cmd/template-mcp-codemode stdio
go run ./cmd/template-mcp-codemode http --addr localhost:8080
TEMPLATE_MCP_CODEMODE_LOG_LEVEL=debug go run ./cmd/template-mcp-codemode stdio
```

A local build reports `template-mcp-codemode dev (none) built unknown`. GoReleaser supplies version, commit, and date for releases.

Both transports log to stderr. STDIO reserves stdout exclusively for JSON-RPC; never write logs or diagnostics there.

CodeMode execution and discovery limits are set programmatically through `mcpserver.Options.Runtime.Limits`. The template does not add limit flags or environment variables. Zero-valued fields receive CodeMode's bounded defaults. See the [configuration reference](docs/docs/configuration.md) for the defaults and option wiring.

## Common tasks

Moon is the task front door:

```sh
moon run root:format       # check formatting
moon run root:format-fix   # apply formatting
moon run root:lint
moon run root:build
moon run root:test
moon run root:check        # formatting, lint, builds, tests, docs, and proxy checks
moon run docs:serve        # documentation preview on http://127.0.0.1:8000
```

CI runs the same aggregate check with:

```sh
moon ci --summary minimal
```

## Container image

The local image path builds the binary into a signed Wolfi package with [melange](https://github.com/chainguard-dev/melange), then assembles a minimal non-root image with [apko](https://github.com/chainguard-dev/apko):

```sh
mise run image-local
docker run --rm template-mcp-codemode:dev --version
```

The image runs as uid/gid 65532 and contains CA certificates and timezone data but no shell. Its default command is `http --addr 0.0.0.0:8080 --insecure` so the demonstration starts without credentials. This is intentionally unauthenticated. Remove `--insecure` and install production authentication and authorization before deployment.

## CI and release configuration

The CI workflows use minimal permissions, pinned external actions, disabled checkout credential persistence, and dependency caches. Documentation builds on pull requests and deploys from the default branch. A scheduled workflow builds and scans the container image and uploads SARIF to GitHub code scanning. Dependabot covers GitHub Actions, both Go modules, and the docs project.

This repository starts from a `0.0.0` Release Please baseline. The first pending release is `0.1.0`; no predecessor release history applies to this repository.

The configured release path is:

1. Release Please maintains a release pull request and creates a version tag plus draft GitHub release after merge.
2. The release dry-run workflow rehearses the GoReleaser binary path and the native-runner melange/apko image path on the release pull request.
3. GoReleaser builds binaries, checksums, and SBOMs without publishing directly. The release workflow validates and uploads them to the draft release.
4. Native runners build signed per-architecture Wolfi packages. apko publishes `ghcr.io/meigma/template-mcp-codemode:vX.Y.Z` as a multi-platform image.
5. The isolated reusable `attest.yml` workflow creates GitHub provenance for binary checksums and the image. The release workflow also creates a keyless Cosign image signature and attaches an SBOM attestation.
6. A human inspects the draft before publication.

Before the first release from a generated project, update the release app credentials, protected-tag bypass, package names, asset patterns, image name, and `ghd.toml` signer workflow. Run the release dry-run workflow before merging that project's first release pull request.

## Documentation

- [Getting started](docs/docs/getting-started.md)
- [Add a capability](docs/docs/how-to/add-a-capability.md)
- [Configuration](docs/docs/configuration.md)
- [Security model](docs/docs/security.md)
- [Canonical CodeMode documentation](https://meigma.github.io/codemode/)
- [Go API](https://pkg.go.dev/github.com/meigma/template-mcp-codemode)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup and pull request expectations.

## Security

See [SECURITY.md](SECURITY.md) for supported versions and private vulnerability reporting. Read the [security model](docs/docs/security.md) before exposing the HTTP transport or adding privileged handlers.

## License

Licensed under either of:

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE))
- MIT License ([LICENSE-MIT](LICENSE-MIT))

at your option (`SPDX-License-Identifier: Apache-2.0 OR MIT`). Unless you state otherwise, a contribution intentionally submitted for inclusion is dual-licensed under those terms.
