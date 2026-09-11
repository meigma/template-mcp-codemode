// Package mcpserver builds the transport-agnostic MCP server for this template.
//
// The server defined here knows nothing about how it is connected to a client:
// the same *mcp.Server is driven by the stdio and http subcommands in
// internal/cli. Keeping transport concerns out of this package is the seam that
// lets a consumer keep one transport and delete the other without ever touching
// the server or its capabilities.
package mcpserver

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/meigma/codemode"
	hostmcp "github.com/meigma/codemode/mcpserver"

	"github.com/meigma/template-mcp-codemode/internal/templateinfo"
)

// Dependencies holds the shared collaborators a real server's capabilities
// need — for example a database handle, an outbound HTTP client, or a config
// struct. It is empty in the template because the demo capability needs
// nothing; add fields here and read them in your registrations (see
// registerRandomInt). Threading dependencies through [Options] keeps the
// server transport-agnostic: the stdio and http subcommands construct them
// and pass them in, the same way for both.
type Dependencies struct{}

// Options configures the template MCP server.
type Options struct {
	// Version is the release version reported in the server implementation info.
	Version string

	// Deps carries the shared dependencies the server's capabilities need. The
	// zero value is valid; the template's demo capability uses none.
	Deps Dependencies

	// Logger receives server diagnostics. Nil selects a text handler writing
	// to [os.Stderr].
	//
	// WARNING: a logger must never write to [os.Stdout]. The stdio transport
	// reserves stdout for the JSON-RPC message stream, so a single log line
	// there corrupts the protocol. Writing to stderr (the default) is safe for
	// every transport.
	Logger *slog.Logger

	// Resolver resolves the trusted invocation subject from host-owned context.
	// There is no default. The stdio command supplies a process-owned
	// [hostmcp.StaticSubject]; the http command supplies [hostmcp.ContextSubject]
	// and installs identity on each request.
	Resolver hostmcp.InvocationResolver

	// Runtime configures the CodeMode catalog, authorizer, and execution
	// budgets. There is no default authorizer: the CLI supplies
	// [github.com/meigma/codemode/authz.AllowAll] for the demo. Zero-valued
	// limit fields receive CodeMode defaults at Build. The template CLI does
	// not expose limit or Rego flags; change Runtime at this composition seam.
	Runtime codemode.Options
}

// New constructs the template MCP server and registers its capabilities.
//
// New is transport-agnostic; callers choose a transport when they run the
// returned server (see internal/cli). The official MCP surface is exactly
// search_api, describe_api, and execute. Diagnostics go to [Options.Logger].
func New(options Options) (*mcp.Server, error) {
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	builder := codemode.New(options.Runtime)
	registerRandomInt(builder, options.Deps)
	service, err := builder.Build()
	if err != nil {
		return nil, fmt.Errorf("build CodeMode runtime: %w", err)
	}

	server, err := hostmcp.New(service, options.Resolver, hostmcp.Options{
		Implementation: &mcp.Implementation{
			Name:    templateinfo.Name,
			Title:   templateinfo.Title,
			Version: options.Version,
		},
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("construct MCP server: %w", err)
	}
	return server, nil
}
