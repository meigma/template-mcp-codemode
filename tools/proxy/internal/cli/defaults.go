package cli

// This file is the zero-config convenience layer for THIS template's layout:
// inside the template repository a bare `mcp-devproxy` builds and serves
// ./cmd/template-mcp. It is kept apart from the generic flag handling so
// extraction to a standalone repository stays clean — delete this file and
// its one call in resolveConfig, and nothing else changes.

import (
	"log/slog"
	"strings"
)

// defaultBuildCommand builds the template server into the cycle's artifact.
const defaultBuildCommand = "go build -o {{artifact}} ./cmd/template-mcp"

// defaultChildTransport is the template server's stdio transport subcommand.
const defaultChildTransport = "stdio"

// defaultWatchDirs returns the template server's source directories.
func defaultWatchDirs() []string { return []string{"cmd", "internal"} }

// defaultChildArgv returns the template server's stdio invocation.
func defaultChildArgv() []string { return []string{artifactToken, defaultChildTransport} }

// applyTemplateDefaults fills each empty config field independently with
// this template's default — a user may override --build and keep the default
// watch directories — and logs every defaulted value so zero-config behavior
// is never silent.
func applyTemplateDefaults(cfg *config, logger *slog.Logger) {
	if strings.TrimSpace(cfg.buildCommand) == "" {
		cfg.buildCommand = defaultBuildCommand
		logger.Info("no --build command given: using the template default",
			"build", cfg.buildCommand)
	}
	if len(cfg.watchDirs) == 0 {
		cfg.watchDirs = defaultWatchDirs()
		logger.Info("no --watch directories given: using the template default",
			"watch", cfg.watchDirs)
	}
	if len(cfg.childArgv) == 0 {
		cfg.childArgv = defaultChildArgv()
		logger.Info(`no child command given after "--": using the template default`,
			"child", cfg.childArgv)
	}
}
