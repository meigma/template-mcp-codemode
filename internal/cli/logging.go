package cli

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/viper"
)

const (
	// Flag (and viper key) names for the persistent logging flags. Shared
	// between registration in NewRootCommand and value retrieval so the two
	// cannot drift.
	logLevelFlag  = "log-level"
	logFormatFlag = "log-format"

	defaultLogLevel  = "info"
	defaultLogFormat = "text"
)

// newLogger builds a [slog.Logger] writing to w at the given level in the given
// format. Level is one of debug, info, warn, error; format is text or json.
// Empty values select the defaults. It returns an error for an unrecognized
// level or format so misconfiguration fails fast at startup.
//
// w is always a stderr-side stream: stdout is reserved for the JSON-RPC channel
// on the stdio transport, so a log line there would corrupt the protocol.
func newLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	lvl, err := parseLogLevel(level)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", defaultLogFormat:
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q (want \"text\" or \"json\")", format)
	}
}

func parseLogLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", defaultLogLevel:
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (want \"debug\", \"info\", \"warn\", or \"error\")", level)
	}
}

// resolveLogger builds the logger from the bound --log-level/--log-format
// configuration, writing to w.
func resolveLogger(vp *viper.Viper, w io.Writer) (*slog.Logger, error) {
	return newLogger(w, vp.GetString(logLevelFlag), vp.GetString(logFormatFlag))
}
