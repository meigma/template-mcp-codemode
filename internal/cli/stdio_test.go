package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestStdioCommandExitsCleanlyOnInputClose guards the two behaviors that the
// stdio path must get right: the protocol is driven over the injected
// Options.In stream (not hardcoded [os.Stdin]), and the client closing that
// stream is a clean shutdown (exit 0), not an error. A finite reader reaches EOF
// after the message, so ExecuteContext must return nil regardless of whether the
// disconnect races with in-flight request handling.
func TestStdioCommandExitsCleanlyOnInputClose(t *testing.T) {
	t.Parallel()

	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize",` +
		`"params":{"protocolVersion":"2025-11-25","capabilities":{},` +
		`"clientInfo":{"name":"test","version":"0"}}}` + "\n"

	root := NewRootCommand(Options{
		In:    strings.NewReader(initialize),
		Out:   io.Discard,
		Err:   io.Discard,
		Viper: viper.New(),
	})
	root.SetArgs([]string{stdioCommandName})

	err := root.ExecuteContext(context.Background())

	require.NoError(t, err, "input close is a clean stdio shutdown")
}

// TestStdioCommandServesMCP drives the stdio command through the injectable
// Options.In/Out seam with a real MCP client on the other end of a pipe pair,
// proving the full protocol round-trip, then closes the input stream — the MCP
// spec's primary stdio shutdown — and requires a clean exit.
func TestStdioCommandServesMCP(t *testing.T) {
	t.Parallel()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done, _ := startStdioCommand(t, inR, outW)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(
		context.Background(),
		&mcp.IOTransport{Reader: outR, Writer: inW},
		nil,
	)
	require.NoError(t, err, "client connect over the command's stdio streams")
	t.Cleanup(func() { _ = session.Close() })

	tools, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err, "tools/list over stdio")
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.Contains(t, names, "random_int")

	require.NoError(t, inW.Close(), "close the command's input stream")

	assert.NoError(t, waitForCommandExit(t, done), "client closing stdin is a clean shutdown")
}

// TestStdioCommandExitsCleanlyOnContextCancel covers the other normal stdio
// shutdown: a cancelled context (SIGINT/SIGTERM in production) must end the
// command without an error.
func TestStdioCommandExitsCleanlyOnContextCancel(t *testing.T) {
	t.Parallel()

	// The input pipe is never written to nor closed, so the server blocks
	// reading until the context cancellation below interrupts it.
	inR, _ := io.Pipe()
	done, cancel := startStdioCommand(t, inR, io.Discard)

	cancel()

	assert.NoError(t, waitForCommandExit(t, done), "context cancellation is a clean shutdown")
}

// startStdioCommand runs the root command's stdio subcommand on the given
// streams in the background. It returns the channel that receives the
// command's exit error and the cancel function for the command context, for
// tests that exercise signal-style shutdown.
func startStdioCommand(t *testing.T, in io.Reader, out io.Writer) (<-chan error, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	root := NewRootCommand(Options{In: in, Out: out})
	root.SetArgs([]string{stdioCommandName})

	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	return done, cancel
}

// waitForCommandExit returns the command's exit error, failing the test if the
// command does not exit before serverExitTimeout.
func waitForCommandExit(t *testing.T, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(serverExitTimeout):
		t.Fatal("the command did not exit before the timeout")
		return nil
	}
}
