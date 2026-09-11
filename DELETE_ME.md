# Set up a repository created from the CodeMode template

This repository was generated from `template-mcp-codemode`. It includes a transport-agnostic CodeMode runtime, STDIO and Streamable HTTP transports, one demo capability, a development proxy, documentation, CI, and release configuration.

Complete this checklist before feature work, then delete this file.

## Template layout

- `cmd/template-mcp-codemode` is the thin executable entry point. `codemode.ServeWorkerAndExit()` is its first statement.
- `internal/cli` constructs the Cobra command tree, resolves configuration, selects trusted subjects, and runs each transport.
- `internal/mcpserver` builds one immutable CodeMode runtime, registers capabilities, and adapts it to the three MCP tools `search_api`, `describe_api`, and `execute`.
- `internal/templateinfo` owns the binary name, client-visible title, and derived environment-variable prefix.
- `tools/proxy` is a nested Go module that rebuilds and swaps the STDIO child during development.

The HTTP command constructs one runtime and MCP server at startup and shares them across all MCP sessions. Keep database pools, clients, and other shared dependencies in `mcpserver.Options.Deps`; do not construct a runtime per session.

## Collect the new identity

Choose each value independently:

| Variable | Template value | Used for |
| --- | --- | --- |
| `OWNER` | `meigma` | Go modules, repository URLs, GHCR image, docs, and signer workflow. |
| `REPO` | `template-mcp-codemode` | Repository name, root module suffix, GHCR image, and docs URLs. |
| `BINARY` | `template-mcp-codemode` | `cmd` directory, executable, build output, release assets, and container entry point. |
| `NAME` | `template-mcp-codemode` | `templateinfo.Name`, Cobra command, MCP implementation name, and environment prefix. |
| `TITLE` | `Meigma CodeMode MCP server template` | `templateinfo.Title` and the client-visible MCP implementation title. |

Derived values:

- Root module: `github.com/OWNER/REPO`
- Proxy module: `github.com/OWNER/REPO/tools/proxy`
- Environment prefix: uppercase `NAME` with hyphens replaced by underscores (`template-mcp-codemode` becomes `TEMPLATE_MCP_CODEMODE`)
- Image: `ghcr.io/OWNER/REPO`

Do not collapse these values into one global replacement. A repository name, binary name, client-visible title, and environment prefix can differ.

## Files to regenerate or reset

Do not blindly rewrite generated or historical files during the identity search:

- Reset `CHANGELOG.md` to one `# Changelog` heading. Release Please writes the new project's history.
- Regenerate `docs/uv.lock` with `uv lock` after changing `docs/pyproject.toml`.
- Let `go mod tidy` update each `go.sum`.
- Ignore generated output such as `bin/`, `coverage.out`, `dist/`, and `docs/build/`.
- Do not rename this file. Delete it after the checklist is complete.

## Rename the project

### 1. Rename both Go modules

The root and development proxy are separate modules:

```sh
go mod edit -module github.com/OWNER/REPO
(cd tools/proxy && go mod edit -module github.com/OWNER/REPO/tools/proxy)
```

Update imports that refer to the template module. Preserve the `github.com/meigma/codemode` dependency and imports; CodeMode is the runtime library, not a template identity surface.

### 2. Rename the binary

```sh
mv cmd/template-mcp-codemode cmd/BINARY
```

Update every build-source and output path, including:

- root `moon.yml`
- `.goreleaser.yaml`
- `melange.yaml`
- `apko.yaml`
- `ghd.toml`
- release and security-scan workflows
- `tools/proxy/internal/cli/defaults.go`
- `.mcp.json` if its invocation changes
- README and documentation commands

### 3. Rename application identity

Update `Name` and `Title` in `internal/templateinfo/info.go`. `Name` controls the Cobra command, MCP implementation name, and environment prefix. `Title` is reported to MCP clients.

Search for every template identity, including human-readable variants:

```sh
rg -i "template-mcp-codemode|TEMPLATE_MCP_CODEMODE|Meigma CodeMode MCP server template|meigma"
```

Map each result to `OWNER`, `REPO`, `BINARY`, `NAME`, or `TITLE`. Update the root and proxy module paths, repository URLs, package names, binary paths, container image, release assets, `ghd.toml` signer workflow, documentation metadata, and environment-variable examples.

Do not replace upstream CodeMode names or links. The fixed adapter default implementation identity `codemode` and its three MCP tool names also belong to the upstream protocol surface, not this repository's brand.

## Preserve worker wiring

CodeMode re-executes the final binary for each program worker. This line must remain the first statement of `main`:

```go
func main() {
	codemode.ServeWorkerAndExit()
	// ordinary host setup follows
}
```

It must precede signal setup, flag parsing, credentials, clients, authorizers, handlers, and transports. Package initialization still runs before `main`, so do not put privileged setup or irreversible side effects in package initializers.

Every test package that calls `Builder.Build` needs:

```go
func TestMain(m *testing.M) {
	codemode.ServeWorkerAndExit()
	os.Exit(m.Run())
}
```

Keep the worker call as the first statement. A test package that never builds a CodeMode server does not need `TestMain`.

## Replace the demo capability

Add your real capabilities before removing `random.int` so the server remains useful throughout the cutover. For each capability:

1. Define non-pointer input and output structs with supported fields and JSON tags.
2. Use `int64` for integer inputs; CodeMode does not accept platform-sized `int` input fields.
3. Register a stable capability ID, dotted name, discovery metadata, and typed handler through `codemode.Register`.
4. Pass shared collaborators through `mcpserver.Dependencies` and close over them in the handler.
5. Add behavior-focused tests and update the expected capability catalog.
6. Delete `randomint.go`, its tests, and its registration after the replacement capabilities are registered.

Do not register each capability as a direct MCP tool. The MCP surface remains exactly `search_api`, `describe_api`, and `execute`.

Keep `codemode.ServeWorkerAndExit` in the final binary and applicable test binaries. Keep the CodeMode module dependency and the `mcpserver.Options.Runtime` construction even after the demo capability is removed.

## Replace demo identity and authorization

The template's identity and policy wiring is explicit:

- STDIO uses `mcpserver.StaticSubject` because local process ownership is its authentication boundary.
- HTTP uses `mcpserver.ContextSubject`. The MCP receiving middleware copies the SDK-authenticated `req.GetExtra().TokenInfo.UserID` into `authz.WithSubject`; an arbitrary value added only to the outer `net/http` request context is not the adapter's identity channel.
- The demo verifier sets `TokenInfo.UserID` to `shared-token`. Loopback or explicit `--insecure` requests without authentication use `development`. These are development identities, not production principals.
- The CLI passes `authz.AllowAll()` so the demo permits every call. `internal/mcpserver.New` has no hidden authorization fallback.

For production HTTP, replace the shared-token verifier with real authentication that sets a stable, non-secret `auth.TokenInfo.UserID`. Keep the receiving-middleware bridge and `mcpserver.ContextSubject`; the bridge installs that ID as an `authz.Subject` with `authz.WithSubject` on the MCP handler context. Replace `AllowAll` with an authorizer appropriate for the enabled capabilities and their canonical arguments. Do not derive identity from Starlark source, MCP tool arguments, `_meta`, unvalidated headers, or arbitrary outer HTTP context values.

Discovery is not authorization-filtered. Every authenticated subject can search and describe every statically enabled capability. Do not place secrets or tenant-sensitive details in discovery metadata; use static capability disabling when a deployment must hide a capability's existence.

## Choose a transport

The template includes both transports:

- Keep STDIO for a server launched as a local subprocess.
- Keep Streamable HTTP for a remote or containerized server.

To remove a transport, delete its file in `internal/cli` and its registration in `internal/cli/root.go`. Capability registration remains in `internal/mcpserver`.

If you keep HTTP, preserve the one-runtime-at-startup design. Do not move `mcpserver.New` into the SDK's per-session factory.

## Configure releases

The template starts at Release Please baseline `0.0.0`; its first pending release is `0.1.0`. A generated project must keep its own changelog and release history.

For a binary plus container release:

- Update `.goreleaser.yaml`: project, build ID, main package, binary, archive names, and package paths.
- Update `ghd.toml`: signer workflow, package name, description, asset patterns, and installed path.
- Update `melange.yaml`: package name, description, Go package, and output.
- Update `apko.yaml`: local package, entry point, command, image annotations, and source URL.
- Update the release, dry-run, and security-scan workflows: image name, binary validation paths, smoke commands, and summaries.
- Update `release-please-config.json` and keep `.release-please-manifest.json` at the intended initial baseline.
- Configure the release GitHub App credentials, protected-tag bypass, and package permissions.

If the project is binary-only, remove the melange/apko jobs, image scan, image configuration, and container required checks. If it is container-only, remove GoReleaser, `ghd.toml`, binary jobs, and binary required checks. Keep the release dry run for every release path that remains.

## Update documentation

Rewrite `README.md` and `docs/docs/` for the real capabilities and retained transports. Update `docs/mkdocs.yml` (`site_url`, `repo_name`, `repo_url`, and `edit_uri`) for the generated repository. Review `CONTRIBUTING.md` and `SECURITY.md` and update the license holder if needed.

Link to the [canonical CodeMode documentation](https://meigma.github.io/codemode/) for the complete runtime, type, MCP tool, and security contracts rather than copying the upstream reference into the generated project.

## Regenerate and verify

Regenerate module and documentation metadata:

```sh
go mod tidy
(cd tools/proxy && go mod tidy)
(cd docs && uv lock)
```

Run the repository gate:

```sh
moon run root:check
```

Then repeat the identity search. It should return no template-owned identity except intentional historical context that you reviewed:

```sh
rg -i "template-mcp-codemode|TEMPLATE_MCP_CODEMODE|Meigma CodeMode MCP server template|meigma"
```

Finally, build the renamed binary and use a real MCP client to call `search_api`, `describe_api`, and `execute` against one replacement capability over the retained transport. Delete this file after those checks pass:

```sh
rm DELETE_ME.md
```
