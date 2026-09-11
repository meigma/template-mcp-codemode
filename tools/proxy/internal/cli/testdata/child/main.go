// Command child is the E2E test fixture the dev proxy builds and supervises:
// a minimal MCP stdio server whose tool set comes from a JSON manifest given
// as its first argument, so editing the manifest in the watched tree is
// indistinguishable from a source change once the proxy rebuilds and respawns
// the binary. At startup it appends its pid to the file given as its second
// argument, letting the tests prove that swapped-out children die and that
// shutdown leaves no orphans.
//
// An optional third argument switches on hang mode for the shutdown
// escalation test: the process ignores SIGTERM — recording each receipt in
// the file the argument names — and refuses to exit when stdin closes, so
// only the proxy's SIGKILL escalation can end it.
//
// It lives under testdata so the module's ./... wildcards (build, lint, test)
// never touch it; the E2E test builds it by explicit path. It compiles inside
// this module against the module's own go.mod, which is what keeps the build
// offline: every required module is already in the local cache because the
// test binary itself was just built from the same graph.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 3 && len(os.Args) != 4 {
		return fmt.Errorf("usage: %s <tools-manifest.json> <pid-file> [<sigterm-marker-file>]", os.Args[0])
	}

	tools, err := readManifest(os.Args[1])
	if err != nil {
		return err
	}
	if err := appendPid(os.Args[2]); err != nil {
		return err
	}
	hang := len(os.Args) == 4
	if hang {
		resistTermination(os.Args[3])
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "e2e-child", Version: "0.0.1"}, nil)
	for _, name := range tools {
		server.AddTool(
			&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "ok-" + name}},
				}, nil
			},
		)
	}
	err = server.Run(context.Background(), &mcp.StdioTransport{})
	if hang {
		// Stdin closed but the process refuses to die: with SIGTERM ignored
		// above, only the transport's SIGKILL escalation step can end it.
		select {}
	}
	return err
}

// resistTermination makes hang mode survive the shutdown ladder's polite
// steps: SIGTERM no longer terminates the process, and each receipt is
// recorded in markerFile — the escalation test's proof that SIGTERM was
// delivered and ignored before SIGKILL ended the process.
func resistTermination(markerFile string) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	go func() {
		for range sigs {
			_ = os.WriteFile(markerFile, []byte("sigterm\n"), 0o600)
		}
	}()
}

// readManifest parses the JSON array of tool names to serve.
func readManifest(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tools manifest: %w", err)
	}
	var tools []string
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("parse tools manifest: %w", err)
	}
	return tools, nil
}

// appendPid records this process in the pid file, one pid per line, oldest
// first.
func appendPid(path string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open pid file: %w", err)
	}
	if _, err := fmt.Fprintln(file, os.Getpid()); err != nil {
		return fmt.Errorf("record pid: %w", err)
	}
	return file.Close()
}
