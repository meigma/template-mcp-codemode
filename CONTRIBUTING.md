# Contributing

This repository is a Go CodeMode MCP server template. Keep changes focused, preserve the generated-project path, and route private vulnerability reports through [SECURITY.md](SECURITY.md).

## Report a bug

Use GitHub issues for non-security bugs. Include, when applicable:

- version, commit, operating system, and architecture;
- steps to reproduce;
- expected and actual behavior; and
- relevant logs or a minimal reproduction.

Do not report a vulnerability in a public issue, pull request, or discussion. Follow [SECURITY.md](SECURITY.md) instead.

## Pull requests

1. Keep the change scoped to one problem.
2. Add or update behavior-focused tests when behavior changes.
3. Update documentation when a user-visible contract changes.
4. Use a Conventional Commit subject, such as `feat: add records capability` or `fix: honor canceled handler context`.
5. Run `moon run root:check` before requesting review.

A capability change must preserve the CodeMode boundary: register it through `codemode.Register`, not as another direct MCP tool. The externally listed MCP tools remain `search_api`, `describe_api`, and `execute`.

## Local setup

Install the pinned Go 1.26.6 toolchain and project tools through [mise](https://mise.jdx.dev):

```sh
mise install
moon run root:check
```

Useful commands:

```sh
moon run root:format
moon run root:format-fix
moon run root:lint
moon run root:build
moon run root:test
moon run docs:serve
go run ./cmd/template-mcp-codemode --version
```

The STDIO server blocks until its client closes input or the process receives a signal. This is expected. macOS does not include `timeout` or `gtimeout` by default; use another time-bounding mechanism or install coreutils when a local script needs one.

## CodeMode worker entry points

`codemode.ServeWorkerAndExit()` must remain the first statement of the final binary's `main`, before flags, credentials, service clients, authorizers, handlers, or transports.

A test package that calls `Builder.Build` must define:

```go
func TestMain(m *testing.M) {
	codemode.ServeWorkerAndExit()
	os.Exit(m.Run())
}
```

The worker call must also be the first statement of `TestMain`. Do not add setup before it. Package initializers run before either function, so keep them free of privileged setup and irreversible side effects.

## Documentation changes

Use the existing Diátaxis page roles:

- `getting-started.md` is the runnable tutorial.
- `how-to/add-a-capability.md` is the repository-specific extension procedure.
- `configuration.md` is the CLI and runtime-options reference.
- `security.md` explains deployment and execution boundaries.

Link to the [canonical CodeMode documentation](https://meigma.github.io/codemode/) instead of duplicating its full public API, Starlark, or MCP tool reference.

## Release changes

Release Please uses Conventional Commit subjects to prepare the changelog and release pull request. This repository starts at baseline `0.0.0`, with `0.1.0` as its first pending release. Do not restore release entries inherited from another repository.

Changes to release configuration must keep the matching dry-run path current. Review binary names, asset patterns, image names, smoke commands, and signer-workflow references together.
