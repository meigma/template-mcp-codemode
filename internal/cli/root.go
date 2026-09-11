// Package cli builds the template-mcp command tree.
//
// The root command wires two transport subcommands onto the same
// transport-agnostic MCP server from internal/mcpserver: stdio, for clients
// that spawn the process and speak JSON-RPC over its standard streams, and
// http, for networked clients using the Streamable HTTP transport. Each
// transport lives in its own file (stdio.go, http.go) so a consumer can keep
// one transport and delete the other by removing a single file and its
// registration in [NewRootCommand].
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/meigma/template-mcp/internal/templateinfo"
)

// BuildInfo describes linker-injected build metadata printed by --version.
type BuildInfo struct {
	// Version is the release version.
	Version string
	// Commit is the source commit used to build the binary.
	Commit string
	// Date is the build timestamp.
	Date string
}

// Options customizes root command construction.
type Options struct {
	// In supplies the command input stream. For the stdio transport this is
	// the client's JSON-RPC message stream.
	In io.Reader
	// Out receives machine-readable command output, including the stdio
	// transport's JSON-RPC messages.
	Out io.Writer
	// Err receives diagnostics and human-readable status, including the MCP
	// server's logs.
	Err io.Writer
	// Build controls the root command version output.
	Build BuildInfo
	// Viper is the configuration instance used by the command tree. Flags are
	// bound to environment variables named after [templateinfo.EnvPrefix],
	// for example TEMPLATE_MCP_ADDR.
	Viper *viper.Viper
}

// NewRootCommand creates the template-mcp Cobra command tree.
//
// The root command does no work on its own; it wires the two transport
// subcommands (stdio and http) onto the same MCP server. To produce a
// single-transport repository, delete the unwanted subcommand file and its
// registration call below.
func NewRootCommand(options Options) *cobra.Command {
	if options.In == nil {
		options.In = strings.NewReader("")
	}
	if options.Out == nil {
		options.Out = io.Discard
	}
	if options.Err == nil {
		options.Err = io.Discard
	}
	if options.Viper == nil {
		options.Viper = viper.New()
	}
	options.Build = options.Build.withDefaults()

	root := &cobra.Command{
		Use:           templateinfo.Name,
		Short:         templateinfo.Title,
		Version:       options.Build.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return initializeConfig(cmd, options.Viper)
		},
	}
	root.SetVersionTemplate(
		fmt.Sprintf(
			"%s %s (%s) built %s\n",
			templateinfo.Name,
			options.Build.Version,
			options.Build.Commit,
			options.Build.Date,
		),
	)
	root.SetIn(options.In)
	root.SetOut(options.Out)
	root.SetErr(options.Err)

	// Persistent logging flags apply to every subcommand and bind to
	// TEMPLATE_MCP_LOG_LEVEL / TEMPLATE_MCP_LOG_FORMAT via initializeConfig.
	// Logs always go to stderr; stdout stays the JSON-RPC channel.
	root.PersistentFlags().String(
		logLevelFlag,
		defaultLogLevel,
		"log level: debug, info, warn, or error (env TEMPLATE_MCP_LOG_LEVEL)",
	)
	root.PersistentFlags().String(
		logFormatFlag,
		defaultLogFormat,
		"log format: text or json (env TEMPLATE_MCP_LOG_FORMAT)",
	)

	root.AddCommand(newStdioCommand(options))
	root.AddCommand(newHTTPCommand(options))

	return root
}

func (b BuildInfo) withDefaults() BuildInfo {
	if strings.TrimSpace(b.Version) == "" {
		b.Version = "dev"
	}
	if strings.TrimSpace(b.Commit) == "" {
		b.Commit = "none"
	}
	if strings.TrimSpace(b.Date) == "" {
		b.Date = "unknown"
	}
	return b
}

func initializeConfig(cmd *cobra.Command, vp *viper.Viper) error {
	vp.SetEnvPrefix(templateinfo.EnvPrefix())
	vp.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	vp.AutomaticEnv()

	if err := vp.BindPFlags(cmd.Root().PersistentFlags()); err != nil {
		return fmt.Errorf("bind persistent flags: %w", err)
	}
	if err := vp.BindPFlags(cmd.Flags()); err != nil {
		return fmt.Errorf("bind flags: %w", err)
	}

	return nil
}
