// Package mcpserver builds the transport-agnostic MCP server for this template.
//
// The server defined here knows nothing about how it is connected to a client:
// the same *mcp.Server is driven by the stdio and http subcommands in
// internal/cli. Keeping transport concerns out of this package is the seam that
// lets a consumer keep one transport and delete the other without ever touching
// the server or its tools.
package mcpserver

import (
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/template-mcp/internal/templateinfo"
)

// Dependencies holds the shared collaborators a real server's tools need — for
// example a database handle, an outbound HTTP client, or a config struct. It is
// empty in the template because the demo tool needs nothing; add fields here and
// read them in your tool registrations (see registerRandomInt). Threading
// dependencies through [Options] keeps the server transport-agnostic: the stdio
// and http subcommands construct them and pass them in, the same way for both.
type Dependencies struct{}

// Options configures the template MCP server.
type Options struct {
	// Version is the release version reported in the server implementation info.
	Version string

	// Deps carries the shared dependencies the server's tools need. The zero
	// value is valid; the template's demo tool uses none.
	Deps Dependencies

	// Logger receives server diagnostics. Nil selects a text handler writing
	// to [os.Stderr].
	//
	// WARNING: a logger must never write to [os.Stdout]. The stdio transport
	// reserves stdout for the JSON-RPC message stream, so a single log line
	// there corrupts the protocol. Writing to stderr (the default) is safe for
	// every transport.
	Logger *slog.Logger
}

// New constructs the template MCP server and registers its tools.
//
// New is transport-agnostic; callers choose a transport when they run the
// returned server (see internal/cli). Diagnostics go to [Options.Logger].
func New(options Options) *mcp.Server {
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    templateinfo.Name,
		Title:   templateinfo.Title,
		Version: options.Version,
	}, &mcp.ServerOptions{
		Logger: logger,
	})

	registerRandomInt(srv, options.Deps)

	return srv
}
