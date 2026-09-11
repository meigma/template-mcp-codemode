package cli

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionFlagPrintsBuildMetadata(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	root := NewRootCommand(Options{
		Out: &stdout,
		Err: &stderr,
		Build: BuildInfo{
			Version: "0.1.0",
			Commit:  "abc1234",
			Date:    "2026-05-08T10:00:00Z",
		},
	})
	root.SetArgs([]string{"--version"})

	err := root.ExecuteContext(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "template-mcp 0.1.0 (abc1234) built 2026-05-08T10:00:00Z\n", stdout.String())
	assert.Empty(t, stderr.String(), "version output must not write to stderr")
}

// TestVersionFlagDefaultsToDevMetadata pins the --version output for a local
// build with no linker-injected metadata: GoReleaser fills these in for real
// releases, but `go run`/`go build` leave them empty, so the command reports
// the dev/none/unknown defaults rather than blanks.
func TestVersionFlagDefaultsToDevMetadata(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	root := NewRootCommand(Options{Out: &stdout, Err: io.Discard})
	root.SetArgs([]string{"--version"})

	require.NoError(t, root.ExecuteContext(context.Background()))
	assert.Equal(t, "template-mcp dev (none) built unknown\n", stdout.String())
}

func TestRootCommandRegistersTransportSubcommands(t *testing.T) {
	t.Parallel()

	root := NewRootCommand(Options{})

	names := make([]string, 0, len(root.Commands()))
	for _, cmd := range root.Commands() {
		names = append(names, cmd.Name())
	}

	assert.Contains(t, names, stdioCommandName)
	assert.Contains(t, names, httpCommandName)
}

func TestHTTPCommandDefaultsAddrToLoopback(t *testing.T) {
	t.Parallel()

	root := NewRootCommand(Options{})
	httpCmd := findSubcommand(t, root, httpCommandName)

	addr, err := httpCmd.Flags().GetString(addrFlag)

	require.NoError(t, err)
	assert.Equal(t, "localhost:8080", addr, "the default bind address must stay loopback")
}

// findSubcommand returns the named direct subcommand of root, failing the test
// when it is not registered.
func findSubcommand(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()

	for _, cmd := range root.Commands() {
		if cmd.Name() == name {
			return cmd
		}
	}

	t.Fatalf("root command is missing the %q subcommand", name)
	return nil
}
