package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/meigma/template-mcp/tools/proxy/internal/reloader"
)

// artifactToken is the placeholder in the build command template replaced
// with the cycle's unique artifact path.
const artifactToken = "{{artifact}}"

// waitDelay bounds how long a finished or cancelled build waits for its I/O
// pipes to close. A killed build command can leave grandchildren (e.g. go
// build's compile/link processes) holding the inherited output pipe; without
// the bound, Wait would block on them and a cancelled Build could not return
// promptly.
const waitDelay = time.Second

// Options configures a Builder for New.
type Options struct {
	// Command is the build command template; every occurrence of
	// {{artifact}} is replaced with the cycle's unique artifact path. The
	// command is split on whitespace — there is no shell, so quoting and
	// arguments containing spaces are not supported. Required, and must
	// reference {{artifact}}.
	Command string

	// Dir is the working directory for the build command. Empty selects
	// the proxy process's working directory.
	Dir string

	// Logger receives operational logs. Nil selects a no-op logger.
	Logger *slog.Logger
}

// Builder is the exec-backed implementation of the reloader.Builder port.
// It owns a temp directory in which every cycle's artifact is created at a
// fresh path; Close removes the directory and everything in it.
type Builder struct {
	argv        []string
	workDir     string
	artifactDir string
	counter     atomic.Int64
	logger      *slog.Logger
}

// New constructs a Builder from options.
//
// The Command template is split on whitespace (no shell interpretation) and
// must reference {{artifact}}: a build that ignores the artifact path can
// never produce the unique per-cycle binary the reload lifecycle depends on.
// New also creates the temp directory that holds the artifacts; the caller
// is expected to Close the Builder to remove it.
func New(options Options) (*Builder, error) {
	argv := strings.Fields(options.Command)
	if len(argv) == 0 {
		return nil, errors.New("a build command is required")
	}
	if !slices.ContainsFunc(argv, func(field string) bool { return strings.Contains(field, artifactToken) }) {
		return nil, fmt.Errorf("the build command must reference %s", artifactToken)
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	artifactDir, err := os.MkdirTemp("", "mcp-devproxy-")
	if err != nil {
		return nil, fmt.Errorf("create artifact directory: %w", err)
	}

	return &Builder{argv: argv, workDir: options.Dir, artifactDir: artifactDir, logger: logger}, nil
}

// Build runs one build cycle with a freshly substituted artifact path.
//
// On success the BuildResult carries the artifact path and the command's
// combined stdout+stderr output. On failure the output rides the returned
// error instead — the reloader core surfaces only the error for a failed
// cycle. Cancelling ctx kills the build command and Build returns promptly.
func (b *Builder) Build(ctx context.Context) (reloader.BuildResult, error) {
	artifact := filepath.Join(b.artifactDir, fmt.Sprintf("child-%d", b.counter.Add(1)))

	argv := make([]string, len(b.argv))
	for i, field := range b.argv {
		argv[i] = strings.ReplaceAll(field, artifactToken, artifact)
	}

	//nolint:gosec // Running the developer-supplied build command is this adapter's purpose.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = b.workDir
	cmd.WaitDelay = waitDelay

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	b.logger.DebugContext(ctx, "running build command", "argv", argv, "artifact", artifact)

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return reloader.BuildResult{}, fmt.Errorf("build cancelled: %w", ctxErr)
		}
		return reloader.BuildResult{}, fmt.Errorf("build command failed: %w\n%s", err, output.String())
	}

	return reloader.BuildResult{Artifact: artifact, Output: output.String()}, nil
}

// Close removes the Builder's artifact directory and every artifact in it.
func (b *Builder) Close() error {
	return os.RemoveAll(b.artifactDir)
}
