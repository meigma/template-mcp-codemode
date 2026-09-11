// Package cli builds the mcp-devproxy command tree.
//
// The proxy is a single root command — no transport subcommands: v1 is
// stdio-downstream-only, and a future HTTP downstream is a new adapter in
// internal/downstream, not a new verb. The generic flags live here; the
// zero-config defaults for this template's layout are deliberately isolated
// in defaults.go so extraction to a standalone repository stays clean.
package cli

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	// appName is the binary and root command name.
	appName = "mcp-devproxy"

	// envPrefix namespaces the environment variables bound to the flags,
	// for example MCP_DEVPROXY_BUILD.
	envPrefix = "MCP_DEVPROXY"

	// artifactToken is the placeholder both command templates must
	// reference. The cli only validates its presence; substitution lives in
	// the build and upstream adapters, which replace every occurrence with
	// the cycle's unique artifact path.
	artifactToken = "{{artifact}}"
)

// Flag (and viper key) names. Shared between flag registration and value
// retrieval so the two cannot drift.
const (
	buildFlag     = "build"
	watchFlag     = "watch"
	dirFlag       = "dir"
	debounceFlag  = "debounce"
	quiesceFlag   = "quiesce"
	terminateFlag = "terminate"
	verboseFlag   = "verbose"
)

// Defaults for the timing flags. Only the three knobs the design's CLI
// contract brackets (--debounce, --quiesce, --terminate) are exposed as
// flags; the core and adapters' remaining knobs (buffer limit and timeout,
// backoff floor and ceiling, health timeout) stay code defaults until a real
// dev loop demands tuning them.
const (
	defaultDebounce  = 300 * time.Millisecond
	defaultQuiesce   = 5 * time.Second
	defaultTerminate = time.Second
)

// BuildInfo describes build metadata printed by --version. The proxy is dev
// tooling and never ships through GoReleaser, so production builds report
// the withDefaults fallbacks.
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
	// In supplies the command input stream: the downstream client's JSON-RPC
	// message stream.
	In io.Reader
	// Out receives the downstream session's JSON-RPC messages. Nothing else
	// may write to it: stdout is the protocol channel.
	Out io.Writer
	// Err receives diagnostics: the proxy's logs and every child's stderr.
	Err io.Writer
	// Build controls the root command version output.
	Build BuildInfo
	// Viper is the configuration instance used by the command tree. Flags
	// are bound to MCP_DEVPROXY_* environment variables; flags take
	// precedence over the environment, which takes precedence over defaults.
	Viper *viper.Viper
}

// launchFunc runs the resolved proxy configuration. Production passes
// launchProxy; the configuration tests substitute a recorder so the full
// flag, environment, defaulting, and validation path is exercised without
// spawning watchers or children.
type launchFunc func(cmd *cobra.Command, cfg config, logger *slog.Logger) error

// NewRootCommand creates the mcp-devproxy Cobra command.
//
// The child command is positional argv after "--"; everything before it is
// flags. Inside this template repository every flag has a working default
// (see defaults.go), so a bare invocation builds and serves
// ./cmd/template-mcp.
func NewRootCommand(options Options) *cobra.Command {
	return newRootCommand(options, launchProxy)
}

func newRootCommand(options Options, launch launchFunc) *cobra.Command {
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
		Use:   appName + " [flags] -- <child argv>",
		Short: "Hot-reloading development proxy for MCP servers",
		Long: "Hot-reloading development proxy for MCP servers.\n\n" +
			"The client connects to the proxy once and keeps that session for the\n" +
			"whole dev loop. The proxy watches the source tree, rebuilds the server\n" +
			"on change, swaps the child process, and re-advertises its tools via\n" +
			"tools/list_changed — no reconnect.\n\n" +
			"The child command after \"--\" is re-run for every reload cycle with\n" +
			"{{artifact}} replaced by that cycle's freshly built binary.",
		Example: "  " + appName + " \\\n" +
			"    --build \"go build -o {{artifact}} ./cmd/template-mcp\" \\\n" +
			"    --watch cmd --watch internal \\\n" +
			"    -- {{artifact}} stdio",
		Version:       options.Build.Version,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return initializeConfig(cmd, options.Viper)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := newLogger(cmd.ErrOrStderr(), options.Viper.GetBool(verboseFlag))
			cfg, err := resolveConfig(options.Viper, args, cmd.ArgsLenAtDash(), logger)
			if err != nil {
				return err
			}
			return launch(cmd, cfg, logger)
		},
	}
	root.SetVersionTemplate(
		fmt.Sprintf(
			"%s %s (%s) built %s\n",
			appName,
			options.Build.Version,
			options.Build.Commit,
			options.Build.Date,
		),
	)
	root.SetIn(options.In)
	root.SetOut(options.Out)
	root.SetErr(options.Err)

	registerFlags(root)

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

// registerFlags declares the generic proxy flags on the root command. Values
// are read back through viper in RunE so flags, environment variables, and
// defaults share one precedence chain.
func registerFlags(root *cobra.Command) {
	flags := root.Flags()
	flags.String(
		buildFlag,
		"",
		"build command template; split on whitespace (no shell, so quoting and arguments "+
			"containing spaces are not supported) and must reference {{artifact}} "+
			"(env "+envPrefix+"_BUILD)",
	)
	// StringArray, not StringSlice: each --watch value is one path, never
	// comma-split. The env override is a whitespace-separated list because
	// viper casts a plain string to []string via strings.Fields.
	flags.StringArray(
		watchFlag,
		nil,
		"directory to watch recursively for source changes; repeatable "+
			"(env "+envPrefix+"_WATCH, whitespace-separated list)",
	)
	flags.String(
		dirFlag,
		"",
		"working directory for the build command; defaults to the current directory "+
			"(env "+envPrefix+"_DIR)",
	)
	flags.Duration(
		debounceFlag,
		defaultDebounce,
		"how long source-change bursts are coalesced before a rebuild starts "+
			"(env "+envPrefix+"_DEBOUNCE)",
	)
	flags.Duration(
		quiesceFlag,
		defaultQuiesce,
		"how long a swap waits for in-flight tool calls on the old child to drain "+
			"(env "+envPrefix+"_QUIESCE)",
	)
	flags.Duration(
		terminateFlag,
		defaultTerminate,
		"how long each child shutdown escalation step (stdin close, SIGTERM, SIGKILL) waits "+
			"(env "+envPrefix+"_TERMINATE)",
	)
	flags.Bool(
		verboseFlag,
		false,
		"enable debug logging on stderr, including build output "+
			"(env "+envPrefix+"_VERBOSE)",
	)
}

// newLogger builds the proxy's one slog logger. Everything it writes goes to
// errOut: stdout is the JSON-RPC protocol channel on both of the proxy's
// hops, and a stray log line there would corrupt the protocol.
func newLogger(errOut io.Writer, verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: level}))
}

func initializeConfig(cmd *cobra.Command, vp *viper.Viper) error {
	vp.SetEnvPrefix(envPrefix)
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
