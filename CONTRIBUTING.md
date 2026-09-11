# Contributing

Thank you for your interest in contributing.
This repository is a Go MCP server template, so changes should keep the generated-project path simple and predictable.
For private vulnerability reporting, use [SECURITY.md](SECURITY.md) instead of public channels.

## Reporting Bugs

Report non-security bugs through GitHub issues.
Include the following details when possible:

- version, commit, or environment details
- steps to reproduce
- expected behavior
- actual behavior
- logs, screenshots, or a minimal reproduction

If you are reporting a security issue, stop and follow [SECURITY.md](SECURITY.md) instead.

## Pull Requests

Contributors should:

1. Keep changes focused and scoped to a single problem.
2. Add or update tests when behavior changes.
3. Update documentation when user-facing behavior changes.
4. Use Conventional Commit subjects, such as `feat: add config loader` or `fix: handle empty input`.
5. Make sure `moon run root:check` passes before requesting review.

## Local Setup

The pinned toolchain (Go, Moon, the dev CLIs, Python + uv for the docs) is
provisioned by [mise](https://mise.jdx.dev) from `mise.toml` + `mise.lock`; Moon
runs every task against those tools as `system` binaries on PATH. Install mise,
then provision the toolchain and run the full check:

```sh
mise install          # provision every pinned tool, honoring mise.lock
moon run root:check    # also builds the docs (needs the mise-provided Python + uv)
```

Useful project commands:

```sh
moon run root:format       # check formatting
moon run root:format-fix   # apply formatting
moon run root:lint
moon run root:build
moon run root:test
moon run docs:serve        # preview the docs at http://127.0.0.1:8000
go run ./cmd/template-mcp --version
```

A few environment notes:

- macOS has no `timeout`/`gtimeout` by default; install coreutils or use a
  different mechanism when scripting time-bounded runs.
- The `stdio` subcommand is a server: it blocks until the client closes its
  input stream or the process is signaled. That is expected, not a hang.

## Release Changes

Release Please reads Conventional Commit subjects to build changelogs and release PRs.
Keep release-impacting commits clear; routine docs, CI, and maintenance commits should use the appropriate non-release type.
