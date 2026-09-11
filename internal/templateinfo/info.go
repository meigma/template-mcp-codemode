// Package templateinfo holds the application's identity constants.
//
// It is the single source of truth for the application name and title: the CLI
// command name, the MCP server implementation name reported to clients, and the
// environment-variable prefix all derive from [Name]. Renaming this template to
// your own project starts here.
package templateinfo

import "strings"

const (
	// Name is the application and binary name. It is used as the root command
	// name, the MCP server implementation name, and the base of the
	// environment-variable prefix (see [EnvPrefix]).
	Name = "template-mcp"
	// Title is the human-readable server title shown to MCP clients.
	Title = "Meigma MCP server template"
)

// EnvPrefix returns the prefix for the application's environment variables,
// for example TEMPLATE_MCP_ADDR. It is derived from [Name] so a rename keeps
// the command name and the environment variables in sync.
func EnvPrefix() string {
	return strings.ToUpper(strings.ReplaceAll(Name, "-", "_"))
}
