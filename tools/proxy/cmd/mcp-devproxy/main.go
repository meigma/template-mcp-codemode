// Command mcp-devproxy is the hot-reloading MCP dev proxy: it keeps one
// client session alive while rebuilding and swapping the in-development MCP
// server behind it.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/meigma/template-mcp/tools/proxy/internal/cli"
)

func main() {
	os.Exit(run())
}

// run wires the signal-aware context and maps any error to exit code 1. The
// proxy is a dev tool with no richer exit-code contract: success is 0,
// everything else is 1, and every error is printed once to stderr.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The proxy is dev tooling and never ships through GoReleaser, so there
	// is no ldflags metadata to inject; BuildInfo's defaults apply.
	root := cli.NewRootCommand(cli.Options{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
	})
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}

	return 0
}
