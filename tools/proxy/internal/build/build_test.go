package build_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/meigma/template-mcp/tools/proxy/internal/build"
	"github.com/meigma/template-mcp/tools/proxy/internal/reloader"
)

// promptReturnBound is the ceiling on how long a cancelled Build may take to
// return: comfortably above the cancellation delay and the adapter's pipe
// wait, comfortably below the fixture script's sleep.
const promptReturnBound = 3 * time.Second

// writeScript writes an executable /bin/sh fixture script and returns its
// path. Scripts receive the substituted artifact path as $1.
func writeScript(t *testing.T, name, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700), "write fixture script %s", name)
	return path
}

// newBuilder constructs a Builder whose artifact directory is removed when
// the test finishes.
func newBuilder(t *testing.T, command string) *build.Builder {
	t.Helper()

	b, err := build.New(build.Options{Command: command})
	require.NoError(t, err, "construct builder")
	t.Cleanup(func() { assert.NoError(t, b.Close(), "close builder") })
	return b
}

func TestBuilderBuild(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command func(t *testing.T) string
		assert  func(t *testing.T, res reloader.BuildResult, err error)
	}{
		{
			name: "successful build produces the artifact and captures output",
			command: func(t *testing.T) string {
				script := writeScript(t, "ok.sh", "echo compiling-output\ntouch \"$1\"\n")
				return script + " {{artifact}}"
			},
			assert: func(t *testing.T, res reloader.BuildResult, err error) {
				require.NoError(t, err, "expected the build to succeed")
				assert.FileExists(t, res.Artifact, "expected the artifact at the substituted path")
				assert.Contains(t, res.Output, "compiling-output", "expected build output on the result")
			},
		},
		{
			name: "failed build attaches its stdout and stderr to the error",
			command: func(t *testing.T) string {
				script := writeScript(t, "boom.sh", "echo progress-on-stdout\necho boom-on-stderr >&2\nexit 1\n")
				return script + " {{artifact}}"
			},
			assert: func(t *testing.T, res reloader.BuildResult, err error) {
				require.Error(t, err, "expected the build to fail")
				assert.Contains(t, err.Error(), "boom-on-stderr",
					"expected the compile stderr to ride the error — the core logs only the error on failure")
				assert.Contains(t, err.Error(), "progress-on-stdout", "expected the compile stdout to ride the error")
				assert.Empty(t, res.Artifact, "expected no artifact on failure")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := newBuilder(t, tt.command(t))
			res, err := b.Build(t.Context())
			tt.assert(t, res, err)
		})
	}
}

func TestBuilderBuildUniqueArtifacts(t *testing.T) {
	t.Parallel()

	b := newBuilder(t, "touch {{artifact}}")

	first, err := b.Build(t.Context())
	require.NoError(t, err, "first build")
	second, err := b.Build(t.Context())
	require.NoError(t, err, "second build")

	assert.NotEqual(t, first.Artifact, second.Artifact,
		"expected each cycle to get a unique artifact path — the running child's binary is never overwritten")
	assert.FileExists(t, first.Artifact, "expected the first artifact to survive the second build")
	assert.FileExists(t, second.Artifact, "expected the second artifact to exist")
}

func TestBuilderBuildCancellation(t *testing.T) {
	t.Parallel()

	script := writeScript(t, "slow.sh", "sleep 5\n")
	b := newBuilder(t, script+" {{artifact}}")

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := b.Build(ctx)
	elapsed := time.Since(start)

	require.Error(t, err, "expected a cancelled build to fail")
	require.ErrorIs(t, err, context.DeadlineExceeded, "expected the error to carry ctx.Err()")
	assert.Less(t, elapsed, promptReturnBound,
		"expected Build to return promptly on cancellation, not wait out the build command")
}

func TestBuilderClose(t *testing.T) {
	t.Parallel()

	b, err := build.New(build.Options{Command: "touch {{artifact}}"})
	require.NoError(t, err, "construct builder")

	res, err := b.Build(t.Context())
	require.NoError(t, err, "build")

	require.NoError(t, b.Close(), "close builder")
	assert.NoFileExists(t, res.Artifact, "expected Close to remove the artifact directory")
}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command string
	}{
		{name: "empty command", command: ""},
		{name: "whitespace-only command", command: "   "},
		{name: "command without the artifact token", command: "go build ./..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := build.New(build.Options{Command: tt.command})
			require.Error(t, err, "expected New to reject the command %q", tt.command)
		})
	}
}
