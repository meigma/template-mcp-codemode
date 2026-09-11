package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/meigma/template-mcp/internal/cli"
)

// GoReleaser injects these values with ldflags during releases. When they are
// left empty (plain go build or go run), cli.BuildInfo substitutes its
// defaults, which keeps the fallback values in one place.
//
//nolint:gochecknoglobals // ldflags injection requires package-level variables.
var (
	version string
	commit  string
	date    string
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	root := cli.NewRootCommand(cli.Options{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
		Build: cli.BuildInfo{
			Version: version,
			Commit:  commit,
			Date:    date,
		},
	})
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}

	return 0
}
