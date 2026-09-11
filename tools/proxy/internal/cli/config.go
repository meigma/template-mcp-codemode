package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// config carries the resolved proxy configuration from the flag/environment
// layer into newProxy. The fields are generic; applyTemplateDefaults fills
// the empty ones with this template's layout before validation.
type config struct {
	// buildCommand is the build command template: whitespace-split, no
	// shell, with {{artifact}} replaced per cycle by the build adapter.
	buildCommand string

	// buildDir is the build command's working directory; empty selects the
	// proxy process's working directory.
	buildDir string

	// watchDirs lists the directories watched recursively for source
	// changes.
	watchDirs []string

	// childArgv is the child command template taken from after "--"; the
	// upstream adapter replaces {{artifact}} per element at every spawn.
	childArgv []string

	// debounce coalesces source-change bursts before a reload cycle starts.
	debounce time.Duration

	// quiesce bounds a swap's wait for in-flight calls on the old child.
	quiesce time.Duration

	// terminate is each child shutdown escalation step's duration.
	terminate time.Duration
}

// resolveConfig reads the bound flag and environment values out of vp, takes
// the child argv from the positional args after "--", applies this
// template's zero-config defaults to whatever is still empty, and validates
// the result.
func resolveConfig(vp *viper.Viper, args []string, argsLenAtDash int, logger *slog.Logger) (config, error) {
	// Cobra reports argsLenAtDash as the count of positional args before
	// "--", or -1 when no "--" was given. Each misuse gets its own message:
	// args before the separator, positional args with no separator at all,
	// and an explicit separator with nothing after it — a user who typed
	// "--" signalled a child command, so silently substituting the template
	// default would ignore their intent.
	switch {
	case argsLenAtDash > 0:
		return config{}, fmt.Errorf(
			`no arguments are allowed before "--" (usage: %s [flags] -- <child argv>)`, appName)
	case argsLenAtDash < 0 && len(args) > 0:
		return config{}, fmt.Errorf(
			`child command must follow "--" (usage: %s [flags] -- <child argv>)`, appName)
	case argsLenAtDash == 0 && len(args) == 0:
		return config{}, fmt.Errorf(
			`a child command is required after "--" (usage: %s [flags] -- <child argv>)`, appName)
	}

	cfg := config{
		buildCommand: vp.GetString(buildFlag),
		buildDir:     vp.GetString(dirFlag),
		watchDirs:    vp.GetStringSlice(watchFlag),
		childArgv:    args,
		debounce:     vp.GetDuration(debounceFlag),
		quiesce:      vp.GetDuration(quiesceFlag),
		terminate:    vp.GetDuration(terminateFlag),
	}
	applyTemplateDefaults(&cfg, logger)
	if err := validateConfig(cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// validateConfig rejects a configuration the adapters could not run.
//
// The build command's own {{artifact}} requirement is enforced by build.New
// and surfaces from newProxy wrapped with the flag name; the child argv's
// token is checked here because upstream.New only requires a non-empty argv —
// a child command that ignores {{artifact}} would run a stale or nonexistent
// path forever, since every cycle's artifact is a fresh unique file.
func validateConfig(cfg config) error {
	if strings.TrimSpace(cfg.buildCommand) == "" {
		return fmt.Errorf("a --%s command is required", buildFlag)
	}
	if len(cfg.watchDirs) == 0 {
		return fmt.Errorf("at least one --%s directory is required", watchFlag)
	}
	if len(cfg.childArgv) == 0 {
		return errors.New(`a child command is required after "--"`)
	}
	if !slices.ContainsFunc(cfg.childArgv, func(field string) bool {
		return strings.Contains(field, artifactToken)
	}) {
		return fmt.Errorf(
			"the child command must reference %s (the rebuilt binary's path changes every cycle)",
			artifactToken)
	}
	return nil
}
